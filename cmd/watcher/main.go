package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"
)

const (
	argocdClusterRefreshAnnotation = "argocd.argoproj.io/refresh"
	argocdSkipReconcileAnnotation  = "argocd.argoproj.io/skip-reconcile"

	loftProjectLabel                  = "loft.sh/project"
	sleepingSinceAnnotation           = "sleepmode.loft.sh/sleeping-since"
	sleepTypeAnnotation               = "sleepmode.loft.sh/sleep-type"
	readyConditionType                = "Ready"
	virtualClusterOnlineConditionType = "VirtualClusterOnline"
	virtualClusterReadyConditionType  = "VirtualClusterReady"

	defaultKubernetesServiceAccountTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	defaultKubernetesServiceAccountCAPath    = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
)

type vciState string

const (
	vciStateUnknown  vciState = "Unknown"
	vciStateSleeping vciState = "Sleeping"
	vciStateWaking   vciState = "Waking"
	vciStateReady    vciState = "Ready"
)

type watcherConfig struct {
	api                          *kubernetesAPI
	pollInterval                 time.Duration
	argocdApplicationNamespace   string
	argocdClusterSecretNamespace string
	clusterSecretNameTemplate    string
	projectNamespacePrefixes     []string
	applicationProjectLabelKey   string
	applicationNameLabelKey      string
	patchApplicationHealth       bool
	applicationHealthPatchMode   string
	sleepingHealthMessage        string
	wakingHealthMessage          string
}

const (
	applicationHealthPatchModeStatus      = "status"
	applicationHealthPatchModeApplication = "application"
)

type kubernetesAPI struct {
	client      *http.Client
	apiBase     string
	bearerToken string
}

type metadata struct {
	Name        string            `json:"name"`
	Namespace   string            `json:"namespace"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
}

type condition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

type virtualClusterStatus struct {
	Phase      string      `json:"phase"`
	Reason     string      `json:"reason"`
	Message    string      `json:"message"`
	Online     *bool       `json:"online"`
	Conditions []condition `json:"conditions"`
}

type virtualClusterInstance struct {
	Metadata metadata             `json:"metadata"`
	Status   virtualClusterStatus `json:"status"`
}

type healthStatus struct {
	Status  string `json:"status"`
	Message string `json:"message"`
}

type applicationStatus struct {
	Health healthStatus `json:"health"`
}

type application struct {
	Metadata metadata          `json:"metadata"`
	Status   applicationStatus `json:"status"`
}

type secret struct {
	Metadata metadata `json:"metadata"`
}

type listResponse[T any] struct {
	Items []T `json:"items"`
}

type apiStatusError struct {
	StatusCode int
	Status     string
	Body       string
}

func (e *apiStatusError) Error() string {
	if e == nil {
		return ""
	}
	if e.Body == "" {
		return e.Status
	}
	return fmt.Sprintf("%s: %s", e.Status, e.Body)
}

func mustEnv(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func parseList(v string) []string {
	if v == "" {
		return nil
	}

	var items []string
	for _, s := range strings.Split(v, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		items = append(items, s)
	}
	return items
}

func inClusterKubernetesAPIBase() (string, error) {
	host := strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_HOST"))
	if host == "" {
		return "", errors.New("KUBERNETES_SERVICE_HOST is not set")
	}

	port := strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_PORT_HTTPS"))
	if port == "" {
		port = strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_PORT"))
	}
	if port == "" {
		port = "443"
	}

	return "https://" + net.JoinHostPort(host, port), nil
}

func newClusterHTTPClient(apiBase, caPath string, timeout time.Duration) (*http.Client, error) {
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		IdleConnTimeout:       30 * time.Second,
	}

	if strings.HasPrefix(strings.ToLower(apiBase), "https://") {
		caPEM, err := os.ReadFile(caPath)
		if err != nil {
			return nil, fmt.Errorf("read cluster CA %q: %w", caPath, err)
		}

		rootCAs, err := x509.SystemCertPool()
		if err != nil || rootCAs == nil {
			rootCAs = x509.NewCertPool()
		}
		if ok := rootCAs.AppendCertsFromPEM(caPEM); !ok {
			return nil, fmt.Errorf("load cluster CA from %q", caPath)
		}

		transport.TLSClientConfig = &tls.Config{
			MinVersion: tls.VersionTLS12,
			RootCAs:    rootCAs,
		}
	}

	return &http.Client{Transport: transport, Timeout: timeout}, nil
}

func loadWatcherConfig() (watcherConfig, error) {
	apiBase := strings.TrimSpace(os.Getenv("WATCH_KUBERNETES_API"))
	if apiBase == "" {
		var err error
		apiBase, err = inClusterKubernetesAPIBase()
		if err != nil {
			return watcherConfig{}, err
		}
	}

	tokenPath := mustEnv("WATCH_TOKEN_PATH", defaultKubernetesServiceAccountTokenPath)
	tokenBytes, err := os.ReadFile(tokenPath)
	if err != nil {
		return watcherConfig{}, fmt.Errorf("read watcher token %q: %w", tokenPath, err)
	}
	token := strings.TrimSpace(string(tokenBytes))
	if token == "" {
		return watcherConfig{}, fmt.Errorf("watcher token %q is empty", tokenPath)
	}

	timeout := 10 * time.Second
	if raw := strings.TrimSpace(os.Getenv("WATCH_KUBERNETES_TIMEOUT")); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			return watcherConfig{}, fmt.Errorf("parse WATCH_KUBERNETES_TIMEOUT: %w", err)
		}
		timeout = parsed
	}

	caPath := mustEnv("WATCH_CA_PATH", defaultKubernetesServiceAccountCAPath)
	client, err := newClusterHTTPClient(apiBase, caPath, timeout)
	if err != nil {
		return watcherConfig{}, err
	}

	pollInterval := 15 * time.Second
	if raw := strings.TrimSpace(os.Getenv("WATCH_POLL_INTERVAL")); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			return watcherConfig{}, fmt.Errorf("parse WATCH_POLL_INTERVAL: %w", err)
		}
		pollInterval = parsed
	}
	if pollInterval <= 0 {
		return watcherConfig{}, errors.New("WATCH_POLL_INTERVAL must be greater than zero")
	}

	argocdNamespace := mustEnv("ARGOCD_NAMESPACE", "argocd")
	appNamespace := mustEnv("ARGOCD_APPLICATION_NAMESPACE", argocdNamespace)
	secretNamespace := mustEnv("ARGOCD_CLUSTER_SECRET_NAMESPACE", argocdNamespace)

	clusterSecretNameTemplate := strings.TrimSpace(os.Getenv("ARGOCD_CLUSTER_SECRET_NAME_TEMPLATE"))
	if clusterSecretNameTemplate == "" {
		return watcherConfig{}, errors.New("ARGOCD_CLUSTER_SECRET_NAME_TEMPLATE is required")
	}

	projectNamespacePrefixes := parseList(mustEnv("WATCH_PROJECT_NAMESPACE_PREFIXES", "p-,loft-p-"))
	if len(projectNamespacePrefixes) == 0 {
		return watcherConfig{}, errors.New("WATCH_PROJECT_NAMESPACE_PREFIXES must contain at least one prefix")
	}

	return watcherConfig{
		api: &kubernetesAPI{
			client:      client,
			apiBase:     apiBase,
			bearerToken: token,
		},
		pollInterval:                 pollInterval,
		argocdApplicationNamespace:   appNamespace,
		argocdClusterSecretNamespace: secretNamespace,
		clusterSecretNameTemplate:    clusterSecretNameTemplate,
		projectNamespacePrefixes:     projectNamespacePrefixes,
		applicationProjectLabelKey:   mustEnv("WATCH_APPLICATION_PROJECT_LABEL", "vclusterProjectId"),
		applicationNameLabelKey:      mustEnv("WATCH_APPLICATION_NAME_LABEL", "vclusterName"),
		patchApplicationHealth:       strings.EqualFold(mustEnv("WATCH_PATCH_APPLICATION_HEALTH", "false"), "true"),
		applicationHealthPatchMode:   applicationHealthPatchModeStatus,
		sleepingHealthMessage:        mustEnv("WATCH_SLEEPING_MESSAGE", "vCluster sleeping"),
		wakingHealthMessage:          mustEnv("WATCH_WAKING_MESSAGE", "vCluster waking"),
	}, nil
}

func (a *kubernetesAPI) request(ctx context.Context, method, path string, query url.Values, contentType string, body []byte) ([]byte, error) {
	targetURL := strings.TrimRight(a.apiBase, "/") + path
	if len(query) > 0 {
		targetURL += "?" + query.Encode()
	}

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, targetURL, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if a.bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+a.bearerToken)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, &apiStatusError{
			StatusCode: resp.StatusCode,
			Status:     resp.Status,
			Body:       strings.TrimSpace(string(data)),
		}
	}

	return data, nil
}

func (a *kubernetesAPI) getJSON(ctx context.Context, path string, query url.Values, out any) error {
	data, err := a.request(ctx, http.MethodGet, path, query, "", nil)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, out); err != nil {
		return err
	}
	return nil
}

func (a *kubernetesAPI) mergePatch(ctx context.Context, path string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = a.request(ctx, http.MethodPatch, path, nil, "application/merge-patch+json", body)
	return err
}

func (a *kubernetesAPI) listVirtualClusterInstances(ctx context.Context) ([]virtualClusterInstance, error) {
	var out listResponse[virtualClusterInstance]
	if err := a.getJSON(ctx, "/apis/management.loft.sh/v1/virtualclusterinstances", nil, &out); err != nil {
		return nil, err
	}
	sort.Slice(out.Items, func(i, j int) bool {
		left := out.Items[i].Metadata.Namespace + "/" + out.Items[i].Metadata.Name
		right := out.Items[j].Metadata.Namespace + "/" + out.Items[j].Metadata.Name
		return left < right
	})
	return out.Items, nil
}

func (a *kubernetesAPI) listApplications(ctx context.Context, namespace, projectLabelKey, project, nameLabelKey, name string) ([]application, error) {
	var out listResponse[application]
	query := url.Values{}
	query.Set("labelSelector", fmt.Sprintf("%s=%s,%s=%s", projectLabelKey, project, nameLabelKey, name))

	path := "/apis/argoproj.io/v1alpha1/namespaces/" + url.PathEscape(namespace) + "/applications"
	if err := a.getJSON(ctx, path, query, &out); err != nil {
		return nil, err
	}

	sort.Slice(out.Items, func(i, j int) bool {
		return out.Items[i].Metadata.Name < out.Items[j].Metadata.Name
	})
	return out.Items, nil
}

func (a *kubernetesAPI) getApplication(ctx context.Context, namespace, name string) (*application, error) {
	var out application
	path := "/apis/argoproj.io/v1alpha1/namespaces/" + url.PathEscape(namespace) + "/applications/" + url.PathEscape(name)
	if err := a.getJSON(ctx, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (a *kubernetesAPI) getSecret(ctx context.Context, namespace, name string) (*secret, error) {
	var out secret
	path := "/api/v1/namespaces/" + url.PathEscape(namespace) + "/secrets/" + url.PathEscape(name)
	if err := a.getJSON(ctx, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (a *kubernetesAPI) patchSecretSkipReconcile(ctx context.Context, namespace, name string, enabled bool) error {
	annotations := map[string]any{}
	if enabled {
		annotations[argocdSkipReconcileAnnotation] = "true"
	} else {
		annotations[argocdSkipReconcileAnnotation] = nil
	}

	return a.mergePatch(ctx, "/api/v1/namespaces/"+url.PathEscape(namespace)+"/secrets/"+url.PathEscape(name), map[string]any{
		"metadata": map[string]any{
			"annotations": annotations,
		},
	})
}

func (a *kubernetesAPI) patchApplicationRefresh(ctx context.Context, namespace, name string) error {
	return a.mergePatch(ctx, "/apis/argoproj.io/v1alpha1/namespaces/"+url.PathEscape(namespace)+"/applications/"+url.PathEscape(name), map[string]any{
		"metadata": map[string]any{
			"annotations": map[string]any{
				argocdClusterRefreshAnnotation: "hard",
			},
		},
	})
}

func (a *kubernetesAPI) patchApplicationHealthStatusSubresource(ctx context.Context, namespace, name, status, message string) error {
	return a.mergePatch(ctx, "/apis/argoproj.io/v1alpha1/namespaces/"+url.PathEscape(namespace)+"/applications/"+url.PathEscape(name)+"/status", map[string]any{
		"status": map[string]any{
			"health": map[string]string{
				"status":  status,
				"message": message,
			},
		},
	})
}

func (a *kubernetesAPI) patchApplicationHealthOnResource(ctx context.Context, namespace, name, status, message string) error {
	return a.mergePatch(ctx, "/apis/argoproj.io/v1alpha1/namespaces/"+url.PathEscape(namespace)+"/applications/"+url.PathEscape(name), map[string]any{
		"status": map[string]any{
			"health": map[string]string{
				"status":  status,
				"message": message,
			},
		},
	})
}

func projectFromVCI(vci virtualClusterInstance, prefixes []string) string {
	if project := strings.TrimSpace(vci.Metadata.Labels[loftProjectLabel]); project != "" {
		return project
	}

	namespace := strings.TrimSpace(vci.Metadata.Namespace)
	for _, prefix := range prefixes {
		if strings.HasPrefix(namespace, prefix) {
			return strings.TrimPrefix(namespace, prefix)
		}
	}

	return ""
}

func clusterSecretName(template, project, name string) string {
	replacer := strings.NewReplacer(
		"{project}", project,
		"{virtualcluster}", name,
	)
	return replacer.Replace(template)
}

func hasSleepAnnotation(annotations map[string]string) bool {
	if annotations == nil {
		return false
	}
	if strings.TrimSpace(annotations[sleepingSinceAnnotation]) != "" {
		return true
	}
	return strings.TrimSpace(annotations[sleepTypeAnnotation]) != ""
}

func findCondition(conditions []condition, conditionType string) (condition, bool) {
	for _, cond := range conditions {
		if cond.Type == conditionType {
			return cond, true
		}
	}
	return condition{}, false
}

func containsSleepHint(values ...string) bool {
	for _, value := range values {
		if strings.Contains(strings.ToLower(strings.TrimSpace(value)), "sleep") {
			return true
		}
	}
	return false
}

func classifyVCI(vci virtualClusterInstance, secretPaused bool) vciState {
	if hasSleepAnnotation(vci.Metadata.Annotations) {
		return vciStateSleeping
	}

	onlineCondition, hasOnlineCondition := findCondition(vci.Status.Conditions, virtualClusterOnlineConditionType)
	if strings.EqualFold(vci.Status.Phase, "Ready") ||
		(vci.Status.Online != nil && *vci.Status.Online) ||
		(hasOnlineCondition && strings.EqualFold(onlineCondition.Status, "True")) {
		return vciStateReady
	}

	sleepConditions := []string{
		virtualClusterOnlineConditionType,
		readyConditionType,
		virtualClusterReadyConditionType,
	}
	for _, conditionType := range sleepConditions {
		cond, ok := findCondition(vci.Status.Conditions, conditionType)
		if !ok {
			continue
		}
		if strings.EqualFold(cond.Status, "True") {
			continue
		}
		if containsSleepHint(cond.Reason, cond.Message) {
			return vciStateSleeping
		}
	}

	if containsSleepHint(
		vci.Status.Phase,
		vci.Status.Reason,
		vci.Status.Message,
		onlineCondition.Reason,
		onlineCondition.Message,
	) {
		if !hasOnlineCondition || !strings.EqualFold(onlineCondition.Status, "True") {
			return vciStateSleeping
		}
	}

	if secretPaused {
		return vciStateWaking
	}

	return vciStateUnknown
}

func applicationHasManagedHealth(app application, cfg watcherConfig) bool {
	switch {
	case app.Status.Health.Status == "Suspended" && app.Status.Health.Message == cfg.sleepingHealthMessage:
		return true
	case app.Status.Health.Status == "Progressing" && app.Status.Health.Message == cfg.wakingHealthMessage:
		return true
	default:
		return false
	}
}

func applicationsNeedReadyRefresh(apps []application, cfg watcherConfig) bool {
	if !cfg.patchApplicationHealth {
		return false
	}

	for _, app := range apps {
		if applicationHasManagedHealth(app, cfg) {
			return true
		}
	}
	return false
}

func disableApplicationHealthPatching(cfg *watcherConfig, reason string) {
	if !cfg.patchApplicationHealth {
		return
	}
	cfg.patchApplicationHealth = false
	log.Printf("disabling application health patching: %s", reason)
}

func patchApplicationHealth(ctx context.Context, cfg *watcherConfig, name, status, message string) error {
	switch cfg.applicationHealthPatchMode {
	case applicationHealthPatchModeApplication:
		return cfg.api.patchApplicationHealthOnResource(ctx, cfg.argocdApplicationNamespace, name, status, message)
	default:
		return cfg.api.patchApplicationHealthStatusSubresource(ctx, cfg.argocdApplicationNamespace, name, status, message)
	}
}

func patchApplicationsHealth(ctx context.Context, cfg *watcherConfig, apps []application, status, message string) error {
	for _, app := range apps {
		if app.Status.Health.Status == status && app.Status.Health.Message == message {
			continue
		}

		if err := patchApplicationHealth(ctx, cfg, app.Metadata.Name, status, message); err != nil {
			var statusErr *apiStatusError
			if errors.As(err, &statusErr) {
				switch statusErr.StatusCode {
				case http.StatusNotFound:
					_, getErr := cfg.api.getApplication(ctx, cfg.argocdApplicationNamespace, app.Metadata.Name)
					if getErr == nil && cfg.applicationHealthPatchMode == applicationHealthPatchModeStatus {
						cfg.applicationHealthPatchMode = applicationHealthPatchModeApplication
						log.Printf("application %s exists but /status patch returned 404; falling back to patching status on the Application resource itself", app.Metadata.Name)

						if fallbackErr := patchApplicationHealth(ctx, cfg, app.Metadata.Name, status, message); fallbackErr == nil {
							log.Printf("set application %s health to %s (%s)", app.Metadata.Name, status, message)
							continue
						} else {
							err = fallbackErr
							if errors.As(err, &statusErr) && (statusErr.StatusCode == http.StatusForbidden || statusErr.StatusCode == http.StatusMethodNotAllowed) {
								disableApplicationHealthPatching(cfg, fmt.Sprintf("fallback status patch for Application %s failed with %s; check Argo CD RBAC, or set WATCH_PATCH_APPLICATION_HEALTH=false.", app.Metadata.Name, statusErr.Status))
								return nil
							}
						}
					}

					var getStatusErr *apiStatusError
					if errors.As(getErr, &getStatusErr) && getStatusErr.StatusCode == http.StatusNotFound {
						log.Printf("application %s disappeared before health patch; skipping", app.Metadata.Name)
						continue
					}
				case http.StatusForbidden, http.StatusMethodNotAllowed:
					disableApplicationHealthPatching(cfg, fmt.Sprintf("status patch for Application %s failed with %s; check Argo CD CRD subresources and RBAC, or set WATCH_PATCH_APPLICATION_HEALTH=false.", app.Metadata.Name, statusErr.Status))
					return nil
				}
			}

			return fmt.Errorf("patch application %s health: %w", app.Metadata.Name, err)
		}
		log.Printf("set application %s health to %s (%s)", app.Metadata.Name, status, message)
	}

	return nil
}

func annotateApplicationsHardRefresh(ctx context.Context, cfg *watcherConfig, apps []application) error {
	for _, app := range apps {
		if strings.TrimSpace(app.Metadata.Annotations[argocdClusterRefreshAnnotation]) == "hard" {
			continue
		}

		if err := cfg.api.patchApplicationRefresh(ctx, cfg.argocdApplicationNamespace, app.Metadata.Name); err != nil {
			return fmt.Errorf("annotate application %s for hard refresh: %w", app.Metadata.Name, err)
		}
		log.Printf("annotated application %s with %s=hard", app.Metadata.Name, argocdClusterRefreshAnnotation)
	}

	return nil
}

func reconcileVCI(ctx context.Context, cfg *watcherConfig, vci virtualClusterInstance) error {
	project := projectFromVCI(vci, cfg.projectNamespacePrefixes)
	if project == "" {
		log.Printf("skipping VCI %s/%s: unable to derive project from label %q or namespace prefixes %v", vci.Metadata.Namespace, vci.Metadata.Name, loftProjectLabel, cfg.projectNamespacePrefixes)
		return nil
	}

	apps, err := cfg.api.listApplications(ctx, cfg.argocdApplicationNamespace, cfg.applicationProjectLabelKey, project, cfg.applicationNameLabelKey, vci.Metadata.Name)
	if err != nil {
		return fmt.Errorf("list applications for project=%s virtualcluster=%s: %w", project, vci.Metadata.Name, err)
	}

	secretName := clusterSecretName(cfg.clusterSecretNameTemplate, project, vci.Metadata.Name)
	clusterSecret, err := cfg.api.getSecret(ctx, cfg.argocdClusterSecretNamespace, secretName)
	if err != nil {
		var statusErr *apiStatusError
		if errors.As(err, &statusErr) && statusErr.StatusCode == http.StatusNotFound {
			clusterSecret = nil
		} else {
			return fmt.Errorf("get cluster secret %s/%s: %w", cfg.argocdClusterSecretNamespace, secretName, err)
		}
	}

	secretPaused := clusterSecret != nil && strings.TrimSpace(clusterSecret.Metadata.Annotations[argocdSkipReconcileAnnotation]) == "true"
	state := classifyVCI(vci, secretPaused)

	switch state {
	case vciStateSleeping:
		if clusterSecret != nil && !secretPaused {
			if err := cfg.api.patchSecretSkipReconcile(ctx, cfg.argocdClusterSecretNamespace, secretName, true); err != nil {
				return fmt.Errorf("pause cluster secret %s/%s: %w", cfg.argocdClusterSecretNamespace, secretName, err)
			}
			log.Printf("marked cluster secret %s/%s with %s=true for sleeping VCI %s/%s", cfg.argocdClusterSecretNamespace, secretName, argocdSkipReconcileAnnotation, vci.Metadata.Namespace, vci.Metadata.Name)
		}
		if cfg.patchApplicationHealth {
			if err := patchApplicationsHealth(ctx, cfg, apps, "Suspended", cfg.sleepingHealthMessage); err != nil {
				return err
			}
		}
	case vciStateWaking:
		if clusterSecret != nil && !secretPaused {
			if err := cfg.api.patchSecretSkipReconcile(ctx, cfg.argocdClusterSecretNamespace, secretName, true); err != nil {
				return fmt.Errorf("pause cluster secret %s/%s during wake: %w", cfg.argocdClusterSecretNamespace, secretName, err)
			}
			log.Printf("kept cluster secret %s/%s paused while VCI %s/%s is waking", cfg.argocdClusterSecretNamespace, secretName, vci.Metadata.Namespace, vci.Metadata.Name)
		}
		if cfg.patchApplicationHealth {
			if err := patchApplicationsHealth(ctx, cfg, apps, "Progressing", cfg.wakingHealthMessage); err != nil {
				return err
			}
		}
	case vciStateReady:
		readyTransition := secretPaused || applicationsNeedReadyRefresh(apps, *cfg)

		if clusterSecret != nil && secretPaused {
			if err := cfg.api.patchSecretSkipReconcile(ctx, cfg.argocdClusterSecretNamespace, secretName, false); err != nil {
				return fmt.Errorf("resume cluster secret %s/%s: %w", cfg.argocdClusterSecretNamespace, secretName, err)
			}
			log.Printf("removed %s from cluster secret %s/%s for ready VCI %s/%s", argocdSkipReconcileAnnotation, cfg.argocdClusterSecretNamespace, secretName, vci.Metadata.Namespace, vci.Metadata.Name)
		}

		if readyTransition {
			if err := annotateApplicationsHardRefresh(ctx, cfg, apps); err != nil {
				return err
			}
		}
	case vciStateUnknown:
		log.Printf("leaving VCI %s/%s unchanged: state classification is unknown", vci.Metadata.Namespace, vci.Metadata.Name)
	}

	return nil
}

func reconcileAll(ctx context.Context, cfg *watcherConfig) error {
	vcis, err := cfg.api.listVirtualClusterInstances(ctx)
	if err != nil {
		return err
	}

	for _, vci := range vcis {
		if err := reconcileVCI(ctx, cfg, vci); err != nil {
			log.Printf("reconcile failed for VCI %s/%s: %v", vci.Metadata.Namespace, vci.Metadata.Name, err)
		}
	}

	return nil
}

func run(ctx context.Context, cfg *watcherConfig) error {
	if err := reconcileAll(ctx, cfg); err != nil {
		log.Printf("initial reconcile failed: %v", err)
	}

	ticker := time.NewTicker(cfg.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := reconcileAll(ctx, cfg); err != nil {
				log.Printf("reconcile loop failed: %v", err)
			}
		}
	}
}

func main() {
	cfg, err := loadWatcherConfig()
	if err != nil {
		log.Fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Printf(
		"watcher polling every %s for VirtualClusterInstances -> apps namespace %s, cluster secrets namespace %s, template %q, patch application health=%v",
		cfg.pollInterval,
		cfg.argocdApplicationNamespace,
		cfg.argocdClusterSecretNamespace,
		cfg.clusterSecretNameTemplate,
		cfg.patchApplicationHealth,
	)

	if err := run(ctx, &cfg); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal(err)
	}
}
