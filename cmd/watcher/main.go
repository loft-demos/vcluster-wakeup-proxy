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
	kargoAuthorizedStageAnnotation = "kargo.akuity.io/authorized-stage"

	loftProjectLabel                  = "loft.sh/project"
	sleepingSinceAnnotation           = "sleepmode.loft.sh/sleeping-since"
	sleepTypeAnnotation               = "sleepmode.loft.sh/sleep-type"
	readyConditionType                = "Ready"
	virtualClusterOnlineConditionType = "VirtualClusterOnline"
	virtualClusterReadyConditionType  = "VirtualClusterReady"

	defaultKubernetesServiceAccountTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	defaultKubernetesServiceAccountCAPath    = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	defaultWakeRetryInterval                 = 30 * time.Second
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
	wakeRequester                *wakeRequester
	pollInterval                 time.Duration
	wakeRetryInterval            time.Duration
	argocdApplicationNamespace   string
	argocdClusterSecretNamespace string
	clusterSecretNameTemplate    string
	projectNamespacePrefixes     []string
	updateVCILastActivityOnWake  bool
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

type wakeRequester struct {
	client           *http.Client
	baseURL          string
	bearerToken      string
	acceptedStatuses map[int]struct{}
}

type watcherRuntime struct {
	observedSyncIntents      map[string]string
	observedRefreshRequests  map[string]string
	observedRevisionWakes    map[string]string
	observedKargoPromotions  map[string]string
	lastWakeAttempt          map[string]time.Time
	lastKnownKargoHealth     map[string]healthStatus
	kargoPromotionsChecked   bool
	kargoPromotionsAvailable bool
}

type metadata struct {
	Name            string            `json:"name"`
	Namespace       string            `json:"namespace"`
	ResourceVersion string            `json:"resourceVersion"`
	Labels          map[string]string `json:"labels"`
	Annotations     map[string]string `json:"annotations"`
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
	Health healthStatus    `json:"health"`
	Sync   applicationSync `json:"sync"`
}

type applicationOperation struct {
	Sync json.RawMessage `json:"sync"`
}

type applicationSync struct {
	Status   string `json:"status"`
	Revision string `json:"revision"`
}

type applicationDestination struct {
	Name string `json:"name"`
}

type applicationSpec struct {
	Destination applicationDestination `json:"destination"`
}

type application struct {
	Metadata  metadata              `json:"metadata"`
	Spec      applicationSpec       `json:"spec"`
	Status    applicationStatus     `json:"status"`
	Operation *applicationOperation `json:"operation,omitempty"`
}

type promotionStep struct {
	Uses string `json:"uses"`
}

type promotionSpec struct {
	Stage string          `json:"stage"`
	Steps []promotionStep `json:"steps"`
}

type promotionStatus struct {
	Phase string `json:"phase"`
}

type promotion struct {
	Metadata metadata        `json:"metadata"`
	Spec     promotionSpec   `json:"spec"`
	Status   promotionStatus `json:"status"`
}

type kargoWakeTrigger struct {
	Apps           []application
	PromotionNames []string
	Fingerprint    string
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

func parseStatusSet(v string) map[int]struct{} {
	set := map[int]struct{}{}
	if v == "" {
		return set
	}

	for _, s := range strings.Split(v, ",") {
		switch strings.TrimSpace(s) {
		case "429":
			set[http.StatusTooManyRequests] = struct{}{}
		case "500":
			set[http.StatusInternalServerError] = struct{}{}
		case "502":
			set[http.StatusBadGateway] = struct{}{}
		case "504":
			set[http.StatusGatewayTimeout] = struct{}{}
		}
	}

	return set
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

func newWakeRequesterFromEnv() (*wakeRequester, error) {
	baseURL := strings.TrimSpace(os.Getenv("WATCH_WAKE_UPSTREAM_BASE"))
	if baseURL == "" {
		return nil, nil
	}

	timeout := 10 * time.Second
	if raw := strings.TrimSpace(os.Getenv("WATCH_WAKE_TIMEOUT")); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("parse WATCH_WAKE_TIMEOUT: %w", err)
		}
		timeout = parsed
	}

	caPath := mustEnv("WATCH_WAKE_CA_PATH", defaultKubernetesServiceAccountCAPath)
	client, err := newClusterHTTPClient(baseURL, caPath, timeout)
	if err != nil {
		return nil, err
	}

	bearerToken := strings.TrimSpace(os.Getenv("WATCH_WAKE_BEARER_TOKEN"))
	if bearerToken == "" {
		if tokenPath := strings.TrimSpace(os.Getenv("WATCH_WAKE_TOKEN_PATH")); tokenPath != "" {
			tokenBytes, err := os.ReadFile(tokenPath)
			if err != nil {
				return nil, fmt.Errorf("read wake token %q: %w", tokenPath, err)
			}
			bearerToken = strings.TrimSpace(string(tokenBytes))
			if bearerToken == "" {
				return nil, fmt.Errorf("wake token %q is empty", tokenPath)
			}
		}
	}

	return &wakeRequester{
		client:           client,
		baseURL:          baseURL,
		bearerToken:      bearerToken,
		acceptedStatuses: parseStatusSet(mustEnv("WATCH_WAKE_SUCCESS_ON", "502,504")),
	}, nil
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

	wakeRequester, err := newWakeRequesterFromEnv()
	if err != nil {
		return watcherConfig{}, err
	}

	wakeRetryInterval := defaultWakeRetryInterval
	if raw := strings.TrimSpace(os.Getenv("WATCH_WAKE_RETRY_INTERVAL")); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			return watcherConfig{}, fmt.Errorf("parse WATCH_WAKE_RETRY_INTERVAL: %w", err)
		}
		if parsed <= 0 {
			return watcherConfig{}, errors.New("WATCH_WAKE_RETRY_INTERVAL must be greater than zero")
		}
		wakeRetryInterval = parsed
	}

	return watcherConfig{
		api: &kubernetesAPI{
			client:      client,
			apiBase:     apiBase,
			bearerToken: token,
		},
		wakeRequester:                wakeRequester,
		pollInterval:                 pollInterval,
		wakeRetryInterval:            wakeRetryInterval,
		argocdApplicationNamespace:   appNamespace,
		argocdClusterSecretNamespace: secretNamespace,
		clusterSecretNameTemplate:    clusterSecretNameTemplate,
		projectNamespacePrefixes:     projectNamespacePrefixes,
		updateVCILastActivityOnWake:  strings.EqualFold(mustEnv("WATCH_UPDATE_VCI_LAST_ACTIVITY_ON_WAKE", "false"), "true"),
		patchApplicationHealth:       !strings.EqualFold(mustEnv("WATCH_PATCH_APPLICATION_HEALTH", "true"), "false"),
		applicationHealthPatchMode:   applicationHealthPatchModeStatus,
		sleepingHealthMessage:        mustEnv("WATCH_SLEEPING_MESSAGE", "vCluster sleeping"),
		wakingHealthMessage:          mustEnv("WATCH_WAKING_MESSAGE", "vCluster waking"),
	}, nil
}

func (w *wakeRequester) Execute(ctx context.Context, project, virtualCluster string) error {
	if w == nil {
		return nil
	}

	targetURL := strings.TrimRight(w.baseURL, "/") +
		"/kubernetes/project/" + url.PathEscape(project) +
		"/virtualcluster/" + url.PathEscape(virtualCluster)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, nil)
	if err != nil {
		return fmt.Errorf("build wake request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if w.bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+w.bearerToken)
	}

	resp, err := w.client.Do(req)
	if err != nil {
		return fmt.Errorf("post wake request %s: %w", targetURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusAccepted {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if _, ok := w.acceptedStatuses[resp.StatusCode]; ok {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return fmt.Errorf("post wake request %s: %s: %s", targetURL, resp.Status, strings.TrimSpace(string(body)))
}

func newWatcherRuntime() *watcherRuntime {
	return &watcherRuntime{
		observedSyncIntents:     map[string]string{},
		observedRefreshRequests: map[string]string{},
		observedRevisionWakes:   map[string]string{},
		observedKargoPromotions: map[string]string{},
		lastWakeAttempt:         map[string]time.Time{},
		lastKnownKargoHealth:    map[string]healthStatus{},
	}
}

func listPromotionsOptional(ctx context.Context, cfg *watcherConfig, runtime *watcherRuntime) ([]promotion, error) {
	if runtime != nil && runtime.kargoPromotionsChecked && !runtime.kargoPromotionsAvailable {
		return nil, nil
	}

	promotions, err := cfg.api.listPromotions(ctx)
	if err != nil {
		var statusErr *apiStatusError
		if errors.As(err, &statusErr) {
			switch statusErr.StatusCode {
			case http.StatusNotFound, http.StatusForbidden, http.StatusMethodNotAllowed:
				if runtime != nil {
					runtime.kargoPromotionsChecked = true
					runtime.kargoPromotionsAvailable = false
				}
				log.Printf("Kargo Promotion discovery unavailable (%s); continuing with Argo-only wake detection", statusErr.Status)
				return nil, nil
			}
		}
		return nil, err
	}

	if runtime != nil {
		runtime.kargoPromotionsChecked = true
		runtime.kargoPromotionsAvailable = true
	}

	return promotions, nil
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

func (a *kubernetesAPI) listApplications(ctx context.Context, namespace string) ([]application, error) {
	var out listResponse[application]

	path := "/apis/argoproj.io/v1alpha1/namespaces/" + url.PathEscape(namespace) + "/applications"
	if err := a.getJSON(ctx, path, nil, &out); err != nil {
		return nil, err
	}

	sort.Slice(out.Items, func(i, j int) bool {
		return out.Items[i].Metadata.Name < out.Items[j].Metadata.Name
	})
	return out.Items, nil
}

func (a *kubernetesAPI) listPromotions(ctx context.Context) ([]promotion, error) {
	var out listResponse[promotion]
	if err := a.getJSON(ctx, "/apis/kargo.akuity.io/v1alpha1/promotions", nil, &out); err != nil {
		return nil, err
	}

	sort.Slice(out.Items, func(i, j int) bool {
		left := out.Items[i].Metadata.Namespace + "/" + out.Items[i].Metadata.Name
		right := out.Items[j].Metadata.Namespace + "/" + out.Items[j].Metadata.Name
		return left < right
	})
	return out.Items, nil
}

func applicationsByDestinationName(apps []application) map[string][]application {
	indexed := make(map[string][]application)
	for _, app := range apps {
		destinationName := strings.TrimSpace(app.Spec.Destination.Name)
		if destinationName == "" {
			continue
		}
		indexed[destinationName] = append(indexed[destinationName], app)
	}
	return indexed
}

func applicationAuthorizedStageKey(app application) string {
	if app.Metadata.Annotations == nil {
		return ""
	}

	raw := strings.TrimSpace(app.Metadata.Annotations[kargoAuthorizedStageAnnotation])
	if raw == "" {
		return ""
	}

	parts := strings.SplitN(raw, ":", 2)
	if len(parts) != 2 {
		return ""
	}

	namespace := strings.TrimSpace(parts[0])
	stage := strings.TrimSpace(parts[1])
	if namespace == "" || stage == "" {
		return ""
	}

	return namespace + "/" + stage
}

func applicationsByAuthorizedStage(apps []application) map[string][]application {
	indexed := make(map[string][]application)
	for _, app := range apps {
		stageKey := applicationAuthorizedStageKey(app)
		if stageKey == "" {
			continue
		}
		indexed[stageKey] = append(indexed[stageKey], app)
	}
	return indexed
}

func promotionStageKey(promotion promotion) string {
	namespace := strings.TrimSpace(promotion.Metadata.Namespace)
	stage := strings.TrimSpace(promotion.Spec.Stage)
	if namespace == "" || stage == "" {
		return ""
	}
	return namespace + "/" + stage
}

func promotionUsesArgoCDUpdate(promotion promotion) bool {
	for _, step := range promotion.Spec.Steps {
		if strings.TrimSpace(step.Uses) == "argocd-update" {
			return true
		}
	}
	return false
}

func promotionIsActive(promotion promotion) bool {
	switch strings.TrimSpace(promotion.Status.Phase) {
	case "Succeeded", "Errored", "Failed", "Aborted", "Canceled", "Cancelled", "Skipped":
		return false
	default:
		return true
	}
}

func kargoWakeTriggersByDestination(apps []application, promotions []promotion) map[string]kargoWakeTrigger {
	stageApps := applicationsByAuthorizedStage(apps)
	type triggerAccumulator struct {
		apps       map[string]application
		promotions map[string]struct{}
	}

	accumulators := map[string]*triggerAccumulator{}
	for _, promotion := range promotions {
		if !promotionIsActive(promotion) || !promotionUsesArgoCDUpdate(promotion) {
			continue
		}

		stageKey := promotionStageKey(promotion)
		if stageKey == "" {
			continue
		}

		matchingApps := stageApps[stageKey]
		if len(matchingApps) == 0 {
			continue
		}

		promotionName := strings.TrimSpace(promotion.Metadata.Name)
		if promotionName == "" {
			continue
		}

		for _, app := range matchingApps {
			destinationName := strings.TrimSpace(app.Spec.Destination.Name)
			if destinationName == "" {
				continue
			}

			accumulator := accumulators[destinationName]
			if accumulator == nil {
				accumulator = &triggerAccumulator{
					apps:       map[string]application{},
					promotions: map[string]struct{}{},
				}
				accumulators[destinationName] = accumulator
			}

			accumulator.apps[app.Metadata.Name] = app
			accumulator.promotions[promotionName] = struct{}{}
		}
	}

	triggers := make(map[string]kargoWakeTrigger, len(accumulators))
	for destinationName, accumulator := range accumulators {
		appsForDestination := make([]application, 0, len(accumulator.apps))
		for _, app := range accumulator.apps {
			appsForDestination = append(appsForDestination, app)
		}
		sort.Slice(appsForDestination, func(i, j int) bool {
			return appsForDestination[i].Metadata.Name < appsForDestination[j].Metadata.Name
		})

		promotionNames := make([]string, 0, len(accumulator.promotions))
		for promotionName := range accumulator.promotions {
			promotionNames = append(promotionNames, promotionName)
		}
		sort.Strings(promotionNames)

		triggers[destinationName] = kargoWakeTrigger{
			Apps:           appsForDestination,
			PromotionNames: promotionNames,
			Fingerprint:    strings.Join(promotionNames, "|"),
		}
	}

	return triggers
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

func (a *kubernetesAPI) patchVirtualClusterInstanceLastActivityStatus(ctx context.Context, namespace, name string, lastActivity int64) error {
	return a.mergePatch(ctx, "/apis/management.loft.sh/v1/namespaces/"+url.PathEscape(namespace)+"/virtualclusterinstances/"+url.PathEscape(name)+"/status", map[string]any{
		"status": map[string]any{
			"sleepModeConfig": map[string]any{
				"status": map[string]any{
					"lastActivity": lastActivity,
				},
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
	message := strings.TrimSpace(app.Status.Health.Message)
	if message == "" {
		return false
	}

	return message == cfg.sleepingHealthMessage || message == cfg.wakingHealthMessage
}

func applicationIsKargoManaged(app application) bool {
	if app.Metadata.Annotations == nil {
		return false
	}
	return strings.TrimSpace(app.Metadata.Annotations[kargoAuthorizedStageAnnotation]) != ""
}

func applicationSyncIntentFingerprint(app application) string {
	if app.Operation == nil {
		return ""
	}

	syncPayload := bytes.TrimSpace(app.Operation.Sync)
	if len(syncPayload) == 0 || bytes.Equal(syncPayload, []byte("null")) {
		return ""
	}

	return string(syncPayload)
}

func applicationsWithSyncIntent(apps []application) []application {
	var filtered []application
	for _, app := range apps {
		if applicationSyncIntentFingerprint(app) == "" {
			continue
		}
		filtered = append(filtered, app)
	}
	return filtered
}

func applicationRefreshRequestFingerprint(app application) string {
	raw := strings.TrimSpace(app.Metadata.Annotations[argocdClusterRefreshAnnotation])
	if raw == "" {
		return ""
	}

	resourceVersion := strings.TrimSpace(app.Metadata.ResourceVersion)
	if resourceVersion == "" {
		return raw
	}

	return raw + "@" + resourceVersion
}

func applicationsWithRefreshRequest(apps []application) []application {
	var filtered []application
	for _, app := range apps {
		if applicationRefreshRequestFingerprint(app) == "" {
			continue
		}
		filtered = append(filtered, app)
	}
	return filtered
}

func applicationRevisionWakeFingerprint(app application) string {
	if applicationSyncIntentFingerprint(app) != "" {
		return ""
	}

	if !strings.EqualFold(strings.TrimSpace(app.Status.Sync.Status), "OutOfSync") {
		return ""
	}

	return strings.TrimSpace(app.Status.Sync.Revision)
}

func applicationsWithRevisionWake(apps []application) []application {
	var filtered []application
	for _, app := range apps {
		if applicationRevisionWakeFingerprint(app) == "" {
			continue
		}
		filtered = append(filtered, app)
	}
	return filtered
}

func newSyncIntentApplications(apps []application, observed map[string]string) []application {
	var filtered []application
	for _, app := range apps {
		fingerprint := applicationSyncIntentFingerprint(app)
		if fingerprint == "" || observed[app.Metadata.Name] == fingerprint {
			continue
		}
		filtered = append(filtered, app)
	}
	return filtered
}

func newRefreshRequestApplications(apps []application, observed map[string]string) []application {
	var filtered []application
	for _, app := range apps {
		fingerprint := applicationRefreshRequestFingerprint(app)
		if fingerprint == "" || observed[app.Metadata.Name] == fingerprint {
			continue
		}
		filtered = append(filtered, app)
	}
	return filtered
}

func newRevisionWakeApplications(apps []application, observed map[string]string) []application {
	var filtered []application
	for _, app := range apps {
		fingerprint := applicationRevisionWakeFingerprint(app)
		if fingerprint == "" || observed[app.Metadata.Name] == fingerprint {
			continue
		}
		filtered = append(filtered, app)
	}
	return filtered
}

func rememberSyncIntentApplications(runtime *watcherRuntime, apps []application) {
	if runtime == nil {
		return
	}

	for _, app := range apps {
		if fingerprint := applicationSyncIntentFingerprint(app); fingerprint != "" {
			runtime.observedSyncIntents[app.Metadata.Name] = fingerprint
			continue
		}
		delete(runtime.observedSyncIntents, app.Metadata.Name)
	}
}

func rememberRefreshRequestApplications(runtime *watcherRuntime, apps []application) {
	if runtime == nil {
		return
	}

	for _, app := range apps {
		if fingerprint := applicationRefreshRequestFingerprint(app); fingerprint != "" {
			runtime.observedRefreshRequests[app.Metadata.Name] = fingerprint
			continue
		}
		delete(runtime.observedRefreshRequests, app.Metadata.Name)
	}
}

func rememberRevisionWakeApplications(runtime *watcherRuntime, apps []application) {
	if runtime == nil {
		return
	}

	for _, app := range apps {
		if fingerprint := applicationRevisionWakeFingerprint(app); fingerprint != "" {
			runtime.observedRevisionWakes[app.Metadata.Name] = fingerprint
			continue
		}
		delete(runtime.observedRevisionWakes, app.Metadata.Name)
	}
}

func forgetCompletedSyncIntentApplications(runtime *watcherRuntime, apps []application) {
	if runtime == nil {
		return
	}

	active := make(map[string]struct{}, len(apps))
	for _, app := range apps {
		if applicationSyncIntentFingerprint(app) == "" {
			continue
		}
		active[app.Metadata.Name] = struct{}{}
	}

	for name := range runtime.observedSyncIntents {
		if _, ok := active[name]; ok {
			continue
		}
		delete(runtime.observedSyncIntents, name)
	}
}

func forgetCompletedRefreshRequestApplications(runtime *watcherRuntime, apps []application) {
	if runtime == nil {
		return
	}

	active := make(map[string]struct{}, len(apps))
	for _, app := range apps {
		if applicationRefreshRequestFingerprint(app) == "" {
			continue
		}
		active[app.Metadata.Name] = struct{}{}
	}

	for name := range runtime.observedRefreshRequests {
		if _, ok := active[name]; ok {
			continue
		}
		delete(runtime.observedRefreshRequests, name)
	}
}

func forgetCompletedRevisionWakeApplications(runtime *watcherRuntime, apps []application) {
	if runtime == nil {
		return
	}

	active := make(map[string]struct{}, len(apps))
	for _, app := range apps {
		if applicationRevisionWakeFingerprint(app) == "" {
			continue
		}
		active[app.Metadata.Name] = struct{}{}
	}

	for name := range runtime.observedRevisionWakes {
		if _, ok := active[name]; ok {
			continue
		}
		delete(runtime.observedRevisionWakes, name)
	}
}

func applicationNames(apps []application) []string {
	names := make([]string, 0, len(apps))
	for _, app := range apps {
		if name := strings.TrimSpace(app.Metadata.Name); name != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func wakeRetryDue(runtime *watcherRuntime, clusterSecretName string, retryInterval time.Duration, now time.Time) bool {
	if runtime == nil {
		return true
	}

	lastAttempt, ok := runtime.lastWakeAttempt[clusterSecretName]
	if !ok || lastAttempt.IsZero() {
		return true
	}

	return now.Sub(lastAttempt) >= retryInterval
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

func patchApplicationHealthValue(ctx context.Context, cfg *watcherConfig, app application, desired healthStatus) error {
	if app.Status.Health.Status == desired.Status && app.Status.Health.Message == desired.Message {
		return nil
	}

	if err := patchApplicationHealth(ctx, cfg, app.Metadata.Name, desired.Status, desired.Message); err != nil {
		var statusErr *apiStatusError
		if errors.As(err, &statusErr) {
			switch statusErr.StatusCode {
			case http.StatusNotFound:
				_, getErr := cfg.api.getApplication(ctx, cfg.argocdApplicationNamespace, app.Metadata.Name)
				if getErr == nil && cfg.applicationHealthPatchMode == applicationHealthPatchModeStatus {
					cfg.applicationHealthPatchMode = applicationHealthPatchModeApplication
					log.Printf("application %s exists but /status patch returned 404; falling back to patching status on the Application resource itself", app.Metadata.Name)

					if fallbackErr := patchApplicationHealth(ctx, cfg, app.Metadata.Name, desired.Status, desired.Message); fallbackErr == nil {
						log.Printf("set application %s health to %s (%s)", app.Metadata.Name, desired.Status, desired.Message)
						return nil
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
					return nil
				}
			case http.StatusForbidden, http.StatusMethodNotAllowed:
				disableApplicationHealthPatching(cfg, fmt.Sprintf("status patch for Application %s failed with %s; check Argo CD CRD subresources and RBAC, or set WATCH_PATCH_APPLICATION_HEALTH=false.", app.Metadata.Name, statusErr.Status))
				return nil
			}
		}

		return fmt.Errorf("patch application %s health: %w", app.Metadata.Name, err)
	}
	log.Printf("set application %s health to %s (%s)", app.Metadata.Name, desired.Status, desired.Message)

	return nil
}

func patchApplicationsHealth(ctx context.Context, cfg *watcherConfig, apps []application, status, message string) error {
	desired := healthStatus{Status: status, Message: message}
	for _, app := range apps {
		if applicationIsKargoManaged(app) {
			continue
		}
		if err := patchApplicationHealthValue(ctx, cfg, app, desired); err != nil {
			return err
		}
	}

	return nil
}

func rememberKargoApplicationsHealth(runtime *watcherRuntime, apps []application, cfg watcherConfig) {
	if runtime == nil {
		return
	}

	for _, app := range apps {
		if !applicationIsKargoManaged(app) {
			continue
		}
		if applicationHasManagedHealth(app, cfg) {
			continue
		}
		switch strings.TrimSpace(app.Status.Health.Status) {
		case "", "Progressing", "Unknown":
			continue
		}
		runtime.lastKnownKargoHealth[app.Metadata.Name] = app.Status.Health
	}
}

func desiredKargoApplicationHealth(runtime *watcherRuntime, app application, cfg watcherConfig, dormantMessage string) (healthStatus, bool) {
	if runtime != nil {
		if desired, ok := runtime.lastKnownKargoHealth[app.Metadata.Name]; ok && strings.TrimSpace(desired.Status) != "" {
			if desired.Status == "Healthy" {
				desired.Message = dormantMessage
			}
			return desired, true
		}
	}

	if applicationHasManagedHealth(app, cfg) && app.Status.Health.Status == "Healthy" {
		return healthStatus{Status: "Healthy", Message: dormantMessage}, true
	}

	return healthStatus{}, false
}

func restoreKargoApplicationsHealth(ctx context.Context, cfg *watcherConfig, runtime *watcherRuntime, apps []application, dormantMessage string) error {
	for _, app := range apps {
		if !applicationIsKargoManaged(app) {
			continue
		}
		if applicationSyncIntentFingerprint(app) != "" {
			continue
		}

		desired, ok := desiredKargoApplicationHealth(runtime, app, *cfg, dormantMessage)
		if !ok {
			continue
		}
		if err := patchApplicationHealthValue(ctx, cfg, app, desired); err != nil {
			return err
		}
		if runtime != nil && strings.TrimSpace(desired.Status) != "" {
			runtime.lastKnownKargoHealth[app.Metadata.Name] = desired
		}
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

func touchVCILastActivityOnWake(ctx context.Context, cfg *watcherConfig, vci virtualClusterInstance, wakeTime time.Time) {
	if cfg == nil || cfg.api == nil || !cfg.updateVCILastActivityOnWake {
		return
	}

	if err := cfg.api.patchVirtualClusterInstanceLastActivityStatus(ctx, vci.Metadata.Namespace, vci.Metadata.Name, wakeTime.Unix()); err != nil {
		log.Printf(
			"best-effort update of VCI %s/%s sleepModeConfig.status.lastActivity failed after wake: %v",
			vci.Metadata.Namespace,
			vci.Metadata.Name,
			err,
		)
		return
	}

	log.Printf(
		"updated VCI %s/%s sleepModeConfig.status.lastActivity to %d after wake",
		vci.Metadata.Namespace,
		vci.Metadata.Name,
		wakeTime.Unix(),
	)
}

func reconcileVCI(ctx context.Context, cfg *watcherConfig, runtime *watcherRuntime, vci virtualClusterInstance, appsByDestination map[string][]application, kargoWakeTriggers map[string]kargoWakeTrigger) error {
	if runtime == nil {
		runtime = newWatcherRuntime()
	}

	project := projectFromVCI(vci, cfg.projectNamespacePrefixes)
	if project == "" {
		log.Printf("skipping VCI %s/%s: unable to derive project from label %q or namespace prefixes %v", vci.Metadata.Namespace, vci.Metadata.Name, loftProjectLabel, cfg.projectNamespacePrefixes)
		return nil
	}

	secretName := clusterSecretName(cfg.clusterSecretNameTemplate, project, vci.Metadata.Name)
	apps := appsByDestination[secretName]
	kargoWakeTrigger := kargoWakeTriggers[secretName]
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
	syncIntentApps := applicationsWithSyncIntent(apps)
	newSyncIntentApps := newSyncIntentApplications(syncIntentApps, runtime.observedSyncIntents)
	refreshRequestApps := applicationsWithRefreshRequest(apps)
	newRefreshRequestApps := newRefreshRequestApplications(refreshRequestApps, runtime.observedRefreshRequests)
	revisionWakeApps := applicationsWithRevisionWake(apps)
	newRevisionWakeApps := newRevisionWakeApplications(revisionWakeApps, runtime.observedRevisionWakes)
	if kargoWakeTrigger.Fingerprint == "" {
		delete(runtime.observedKargoPromotions, secretName)
	}

	switch state {
	case vciStateSleeping:
		rememberKargoApplicationsHealth(runtime, apps, *cfg)
		if clusterSecret != nil && !secretPaused {
			if err := cfg.api.patchSecretSkipReconcile(ctx, cfg.argocdClusterSecretNamespace, secretName, true); err != nil {
				return fmt.Errorf("pause cluster secret %s/%s: %w", cfg.argocdClusterSecretNamespace, secretName, err)
			}
			log.Printf("marked cluster secret %s/%s with %s=true for sleeping VCI %s/%s", cfg.argocdClusterSecretNamespace, secretName, argocdSkipReconcileAnnotation, vci.Metadata.Namespace, vci.Metadata.Name)
		}
		if cfg.wakeRequester != nil {
			now := time.Now()
			shouldWake := false
			triggerApps := []application(nil)
			triggerReason := ""

			if kargoWakeTrigger.Fingerprint != "" {
				if runtime.observedKargoPromotions[secretName] != kargoWakeTrigger.Fingerprint ||
					wakeRetryDue(runtime, secretName, cfg.wakeRetryInterval, now) {
					shouldWake = true
					triggerApps = kargoWakeTrigger.Apps
					triggerReason = "active Kargo Promotions " + strings.Join(kargoWakeTrigger.PromotionNames, ", ")
				}
			}

			if !shouldWake && len(syncIntentApps) > 0 {
				if len(newSyncIntentApps) > 0 || wakeRetryDue(runtime, secretName, cfg.wakeRetryInterval, now) {
					shouldWake = true
					triggerApps = newSyncIntentApps
					if len(triggerApps) == 0 {
						triggerApps = syncIntentApps
					}
					triggerReason = "sync intent on applications " + strings.Join(applicationNames(triggerApps), ", ")
				}
			}

			if !shouldWake && len(refreshRequestApps) > 0 {
				if len(newRefreshRequestApps) > 0 || wakeRetryDue(runtime, secretName, cfg.wakeRetryInterval, now) {
					shouldWake = true
					triggerApps = newRefreshRequestApps
					if len(triggerApps) == 0 {
						triggerApps = refreshRequestApps
					}
					triggerReason = "refresh request on applications " + strings.Join(applicationNames(triggerApps), ", ")
				}
			}

			if !shouldWake && len(revisionWakeApps) > 0 {
				if len(newRevisionWakeApps) > 0 || wakeRetryDue(runtime, secretName, cfg.wakeRetryInterval, now) {
					shouldWake = true
					triggerApps = newRevisionWakeApps
					if len(triggerApps) == 0 {
						triggerApps = revisionWakeApps
					}
					triggerReason = "new OutOfSync desired revision on applications " + strings.Join(applicationNames(triggerApps), ", ")
				}
			}

			if shouldWake {
				if err := cfg.wakeRequester.Execute(ctx, project, vci.Metadata.Name); err != nil {
					return fmt.Errorf(
						"wake sleeping VCI %s/%s from %s: %w",
						vci.Metadata.Namespace,
						vci.Metadata.Name,
						triggerReason,
						err,
					)
				}

				touchVCILastActivityOnWake(ctx, cfg, vci, now)
				runtime.lastWakeAttempt[secretName] = now
				rememberSyncIntentApplications(runtime, syncIntentApps)
				rememberRefreshRequestApplications(runtime, refreshRequestApps)
				rememberRevisionWakeApplications(runtime, revisionWakeApps)
				if kargoWakeTrigger.Fingerprint != "" {
					runtime.observedKargoPromotions[secretName] = kargoWakeTrigger.Fingerprint
				}
				log.Printf(
					"triggered wake for sleeping VCI %s/%s due to %s",
					vci.Metadata.Namespace,
					vci.Metadata.Name,
					triggerReason,
				)
			}
		}
		if cfg.patchApplicationHealth {
			if err := patchApplicationsHealth(ctx, cfg, apps, "Suspended", cfg.sleepingHealthMessage); err != nil {
				return err
			}
			if err := restoreKargoApplicationsHealth(ctx, cfg, runtime, apps, cfg.sleepingHealthMessage); err != nil {
				return err
			}
		}
	case vciStateWaking:
		rememberSyncIntentApplications(runtime, syncIntentApps)
		rememberRefreshRequestApplications(runtime, refreshRequestApps)
		rememberRevisionWakeApplications(runtime, revisionWakeApps)
		rememberKargoApplicationsHealth(runtime, apps, *cfg)
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
			if err := restoreKargoApplicationsHealth(ctx, cfg, runtime, apps, cfg.wakingHealthMessage); err != nil {
				return err
			}
		}
	case vciStateReady:
		rememberKargoApplicationsHealth(runtime, apps, *cfg)
		readyTransition := secretPaused || applicationsNeedReadyRefresh(apps, *cfg)
		rememberSyncIntentApplications(runtime, syncIntentApps)
		rememberRefreshRequestApplications(runtime, refreshRequestApps)
		rememberRevisionWakeApplications(runtime, revisionWakeApps)

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
		if cfg.patchApplicationHealth {
			if err := restoreKargoApplicationsHealth(ctx, cfg, runtime, apps, ""); err != nil {
				return err
			}
		}
		delete(runtime.lastWakeAttempt, secretName)
	case vciStateUnknown:
		log.Printf("leaving VCI %s/%s unchanged: state classification is unknown", vci.Metadata.Namespace, vci.Metadata.Name)
	}

	return nil
}

func reconcileAll(ctx context.Context, cfg *watcherConfig, runtime *watcherRuntime) error {
	vcis, err := cfg.api.listVirtualClusterInstances(ctx)
	if err != nil {
		return err
	}

	apps, err := cfg.api.listApplications(ctx, cfg.argocdApplicationNamespace)
	if err != nil {
		return fmt.Errorf("list applications in namespace %s: %w", cfg.argocdApplicationNamespace, err)
	}
	forgetCompletedSyncIntentApplications(runtime, apps)
	forgetCompletedRefreshRequestApplications(runtime, apps)
	forgetCompletedRevisionWakeApplications(runtime, apps)
	appsByDestination := applicationsByDestinationName(apps)
	promotions, err := listPromotionsOptional(ctx, cfg, runtime)
	if err != nil {
		log.Printf("list Kargo Promotions failed; continuing with Argo-only wake detection: %v", err)
	}
	kargoWakeTriggers := kargoWakeTriggersByDestination(apps, promotions)

	for _, vci := range vcis {
		if err := reconcileVCI(ctx, cfg, runtime, vci, appsByDestination, kargoWakeTriggers); err != nil {
			log.Printf("reconcile failed for VCI %s/%s: %v", vci.Metadata.Namespace, vci.Metadata.Name, err)
		}
	}

	return nil
}

func run(ctx context.Context, cfg *watcherConfig) error {
	runtime := newWatcherRuntime()

	if err := reconcileAll(ctx, cfg, runtime); err != nil {
		log.Printf("initial reconcile failed: %v", err)
	}

	ticker := time.NewTicker(cfg.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := reconcileAll(ctx, cfg, runtime); err != nil {
				log.Printf("reconcile loop failed: %v", err)
			}
		}
	}
}

func describeWakeSources(cfg watcherConfig) string {
	if cfg.wakeRequester == nil {
		return "disabled"
	}

	sources := []string{
		"kargo-promotions(auto-detect cluster-wide)",
		"argocd-sync",
		"argocd-refresh-request",
		"argocd-outofsync-revision",
	}
	if cfg.updateVCILastActivityOnWake {
		sources = append(sources, "vci-lastActivity-status-touch-on-wake")
	}

	return strings.Join(sources, ",")
}

func main() {
	cfg, err := loadWatcherConfig()
	if err != nil {
		log.Fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Printf(
		"watcher polling every %s for VirtualClusterInstances -> apps namespace %s, cluster secrets namespace %s, template %q, patch application health=%v, wake sources=%s",
		cfg.pollInterval,
		cfg.argocdApplicationNamespace,
		cfg.argocdClusterSecretNamespace,
		cfg.clusterSecretNameTemplate,
		cfg.patchApplicationHealth,
		describeWakeSources(cfg),
	)

	if err := run(ctx, &cfg); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal(err)
	}
}
