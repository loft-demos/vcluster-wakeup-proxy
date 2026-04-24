package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestProjectFromVCIUsesProjectLabelFirst(t *testing.T) {
	vci := virtualClusterInstance{
		Metadata: metadata{
			Namespace: "p-wrong",
			Labels: map[string]string{
				loftProjectLabel: "right-project",
			},
		},
	}

	if got := projectFromVCI(vci, []string{"p-", "loft-p-"}); got != "right-project" {
		t.Fatalf("expected project label to win, got %q", got)
	}
}

func TestProjectFromVCIDerivesProjectFromNamespacePrefixes(t *testing.T) {
	tests := []struct {
		name      string
		namespace string
		want      string
	}{
		{name: "short prefix", namespace: "p-default", want: "default"},
		{name: "loft prefix", namespace: "loft-p-demo", want: "demo"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vci := virtualClusterInstance{Metadata: metadata{Namespace: tt.namespace}}
			if got := projectFromVCI(vci, []string{"p-", "loft-p-"}); got != tt.want {
				t.Fatalf("expected %q, got %q", tt.want, got)
			}
		})
	}
}

func TestClassifyVCISleepingWhenSleepAnnotationsArePresent(t *testing.T) {
	vci := virtualClusterInstance{
		Metadata: metadata{
			Annotations: map[string]string{
				sleepingSinceAnnotation: "1711800000",
			},
		},
		Status: virtualClusterStatus{
			Phase: "Ready",
			Conditions: []condition{
				{Type: virtualClusterOnlineConditionType, Status: "True"},
			},
		},
	}

	if got := classifyVCI(vci, false); got != vciStateSleeping {
		t.Fatalf("expected Sleeping, got %s", got)
	}
}

func TestClassifyVCIReadyWhenOnlineConditionIsTrue(t *testing.T) {
	vci := virtualClusterInstance{
		Status: virtualClusterStatus{
			Conditions: []condition{
				{Type: virtualClusterOnlineConditionType, Status: "True"},
			},
		},
	}

	if got := classifyVCI(vci, true); got != vciStateReady {
		t.Fatalf("expected Ready, got %s", got)
	}
}

func TestClassifyVCIReadyWhenStatusOnlineIsTrue(t *testing.T) {
	online := true
	vci := virtualClusterInstance{
		Status: virtualClusterStatus{
			Online: &online,
		},
	}

	if got := classifyVCI(vci, false); got != vciStateReady {
		t.Fatalf("expected Ready, got %s", got)
	}
}

func TestClassifyVCISleepingWhenOnlineConditionFalseAndSleepHintPresent(t *testing.T) {
	vci := virtualClusterInstance{
		Status: virtualClusterStatus{
			Phase:   "Sleeping",
			Message: "virtual cluster is sleeping",
			Conditions: []condition{
				{
					Type:    virtualClusterOnlineConditionType,
					Status:  "False",
					Message: "cluster sleeping",
				},
			},
		},
	}

	if got := classifyVCI(vci, false); got != vciStateSleeping {
		t.Fatalf("expected Sleeping, got %s", got)
	}
}

func TestClassifyVCIReadyForObservedAwakeShape(t *testing.T) {
	online := true
	vci := virtualClusterInstance{
		Metadata: metadata{
			Namespace: "p-api-framework",
			Annotations: map[string]string{
				"sleepmode.loft.sh/current-epoch-slept": "51523",
				"sleepmode.loft.sh/scheduled-wakeup":    "1774883460",
			},
		},
		Status: virtualClusterStatus{
			Phase:  "Ready",
			Online: &online,
			Conditions: []condition{
				{Type: readyConditionType, Status: "True"},
				{Type: virtualClusterOnlineConditionType, Status: "True"},
				{Type: virtualClusterReadyConditionType, Status: "True"},
			},
		},
	}

	if got := classifyVCI(vci, true); got != vciStateReady {
		t.Fatalf("expected Ready, got %s", got)
	}
}

func TestClassifyVCISleepingWhenVirtualClusterReadyConditionSaysSleeping(t *testing.T) {
	vci := virtualClusterInstance{
		Status: virtualClusterStatus{
			Conditions: []condition{
				{
					Type:    readyConditionType,
					Status:  "False",
					Reason:  "Sleeping",
					Message: "Virtual Cluster is sleeping",
				},
				{
					Type:    virtualClusterOnlineConditionType,
					Status:  "False",
					Reason:  "NetworkPeerOffline",
					Message: "vCluster seems to be offline",
				},
				{
					Type:    virtualClusterReadyConditionType,
					Status:  "False",
					Reason:  "Sleeping",
					Message: "Virtual Cluster is sleeping",
				},
			},
		},
	}

	if got := classifyVCI(vci, false); got != vciStateSleeping {
		t.Fatalf("expected Sleeping, got %s", got)
	}
}

func TestClassifyVCIWakingWhenPausedAndNotOtherwiseReadyOrSleeping(t *testing.T) {
	vci := virtualClusterInstance{
		Status: virtualClusterStatus{
			Phase: "Starting",
			Conditions: []condition{
				{Type: virtualClusterOnlineConditionType, Status: "False"},
			},
		},
	}

	if got := classifyVCI(vci, true); got != vciStateWaking {
		t.Fatalf("expected Waking, got %s", got)
	}
}

func TestApplicationsNeedReadyRefreshOnlyForManagedHealth(t *testing.T) {
	cfg := watcherConfig{
		patchApplicationHealth: true,
		sleepingHealthMessage:  "vCluster sleeping",
		wakingHealthMessage:    "vCluster waking",
	}

	apps := []application{
		{
			Status: applicationStatus{
				Health: healthStatus{
					Status:  "Suspended",
					Message: "vCluster sleeping",
				},
			},
		},
	}

	if !applicationsNeedReadyRefresh(apps, cfg) {
		t.Fatal("expected ready refresh for managed health")
	}

	apps[0].Status.Health.Status = "Healthy"
	if !applicationsNeedReadyRefresh(apps, cfg) {
		t.Fatal("expected ready refresh for stale managed health message")
	}

	apps[0].Status.Health.Message = "manual override"
	if applicationsNeedReadyRefresh(apps, cfg) {
		t.Fatal("did not expect ready refresh for unrelated app health")
	}
}

func TestPatchApplicationsHealthSkipsKargoManagedApps(t *testing.T) {
	var patched []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Fatalf("expected PATCH request, got %s", r.Method)
		}
		patched = append(patched, r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := watcherConfig{
		api: &kubernetesAPI{
			client:      server.Client(),
			apiBase:     server.URL,
			bearerToken: "token",
		},
		argocdApplicationNamespace: "argocd",
		patchApplicationHealth:     true,
		applicationHealthPatchMode: applicationHealthPatchModeStatus,
	}

	apps := []application{
		{Metadata: metadata{Name: "plain-app"}},
		{
			Metadata: metadata{
				Name: "kargo-app",
				Annotations: map[string]string{
					kargoAuthorizedStageAnnotation: "demo:pre-prod",
				},
			},
		},
	}

	if err := patchApplicationsHealth(context.Background(), &cfg, apps, "Suspended", "vCluster sleeping"); err != nil {
		t.Fatalf("unexpected error patching health: %v", err)
	}

	if len(patched) != 1 {
		t.Fatalf("expected exactly one health patch, got %d", len(patched))
	}
	if !strings.HasSuffix(patched[0], "/applications/plain-app/status") {
		t.Fatalf("expected only non-Kargo app to be patched, got %q", patched[0])
	}
}

func TestRestoreKargoApplicationsHealthUsesLastKnownHealthyStateWithDormantMessage(t *testing.T) {
	var patchedBodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Fatalf("expected PATCH request, got %s", r.Method)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read patch body: %v", err)
		}
		patchedBodies = append(patchedBodies, string(body))
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := watcherConfig{
		api: &kubernetesAPI{
			client:      server.Client(),
			apiBase:     server.URL,
			bearerToken: "token",
		},
		argocdApplicationNamespace: "argocd",
		patchApplicationHealth:     true,
		applicationHealthPatchMode: applicationHealthPatchModeStatus,
		sleepingHealthMessage:      "vCluster sleeping",
		wakingHealthMessage:        "vCluster waking",
	}
	runtime := newWatcherRuntime()
	runtime.lastKnownKargoHealth["kargo-app"] = healthStatus{Status: "Healthy"}

	apps := []application{
		{
			Metadata: metadata{
				Name: "kargo-app",
				Annotations: map[string]string{
					kargoAuthorizedStageAnnotation: "demo:pre-prod",
				},
			},
			Status: applicationStatus{
				Health: healthStatus{
					Status:  "Progressing",
					Message: "vCluster sleeping",
				},
			},
		},
	}

	if err := restoreKargoApplicationsHealth(context.Background(), &cfg, runtime, apps, "vCluster sleeping"); err != nil {
		t.Fatalf("unexpected restore error: %v", err)
	}

	if len(patchedBodies) != 1 {
		t.Fatalf("expected one Kargo health restore patch, got %d", len(patchedBodies))
	}
	if !strings.Contains(patchedBodies[0], `"status":"Healthy"`) {
		t.Fatalf("expected restore patch to set Healthy status, got %s", patchedBodies[0])
	}
	if !strings.Contains(patchedBodies[0], `"message":"vCluster sleeping"`) {
		t.Fatalf("expected restore patch to carry dormancy message, got %s", patchedBodies[0])
	}
}

func TestRestoreKargoApplicationsHealthSkipsActiveSyncIntent(t *testing.T) {
	var patched bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		patched = true
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := watcherConfig{
		api: &kubernetesAPI{
			client:      server.Client(),
			apiBase:     server.URL,
			bearerToken: "token",
		},
		argocdApplicationNamespace: "argocd",
		patchApplicationHealth:     true,
		applicationHealthPatchMode: applicationHealthPatchModeStatus,
		sleepingHealthMessage:      "vCluster sleeping",
		wakingHealthMessage:        "vCluster waking",
	}
	runtime := newWatcherRuntime()
	runtime.lastKnownKargoHealth["kargo-app"] = healthStatus{Status: "Healthy"}

	apps := []application{
		{
			Metadata: metadata{
				Name: "kargo-app",
				Annotations: map[string]string{
					kargoAuthorizedStageAnnotation: "demo:pre-prod",
				},
			},
			Status: applicationStatus{
				Health: healthStatus{
					Status:  "Progressing",
					Message: "vCluster sleeping",
				},
			},
			Operation: &applicationOperation{
				Sync: json.RawMessage(`{"revision":"abc123"}`),
			},
		},
	}

	if err := restoreKargoApplicationsHealth(context.Background(), &cfg, runtime, apps, "vCluster sleeping"); err != nil {
		t.Fatalf("unexpected restore error: %v", err)
	}
	if patched {
		t.Fatal("did not expect Kargo health restore while sync intent is active")
	}
}

func TestRestoreKargoApplicationsHealthClearsDormantMessageWhenReady(t *testing.T) {
	var patchedBodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read patch body: %v", err)
		}
		patchedBodies = append(patchedBodies, string(body))
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := watcherConfig{
		api: &kubernetesAPI{
			client:      server.Client(),
			apiBase:     server.URL,
			bearerToken: "token",
		},
		argocdApplicationNamespace: "argocd",
		patchApplicationHealth:     true,
		applicationHealthPatchMode: applicationHealthPatchModeStatus,
		sleepingHealthMessage:      "vCluster sleeping",
		wakingHealthMessage:        "vCluster waking",
	}
	runtime := newWatcherRuntime()
	runtime.lastKnownKargoHealth["kargo-app"] = healthStatus{Status: "Healthy"}

	apps := []application{
		{
			Metadata: metadata{
				Name: "kargo-app",
				Annotations: map[string]string{
					kargoAuthorizedStageAnnotation: "demo:pre-prod",
				},
			},
			Status: applicationStatus{
				Health: healthStatus{
					Status:  "Healthy",
					Message: "vCluster sleeping",
				},
			},
		},
	}

	if err := restoreKargoApplicationsHealth(context.Background(), &cfg, runtime, apps, ""); err != nil {
		t.Fatalf("unexpected restore error: %v", err)
	}

	if len(patchedBodies) != 1 {
		t.Fatalf("expected one ready-state restore patch, got %d", len(patchedBodies))
	}
	if !strings.Contains(patchedBodies[0], `"status":"Healthy"`) || !strings.Contains(patchedBodies[0], `"message":""`) {
		t.Fatalf("expected ready-state restore patch to clear dormancy message, got %s", patchedBodies[0])
	}
}

func TestRememberKargoApplicationsHealthSkipsTransientProgressingState(t *testing.T) {
	runtime := newWatcherRuntime()
	runtime.lastKnownKargoHealth["kargo-app"] = healthStatus{Status: "Healthy"}

	apps := []application{
		{
			Metadata: metadata{
				Name: "kargo-app",
				Annotations: map[string]string{
					kargoAuthorizedStageAnnotation: "demo:pre-prod",
				},
			},
			Status: applicationStatus{
				Health: healthStatus{
					Status: "Progressing",
				},
			},
		},
	}

	rememberKargoApplicationsHealth(runtime, apps, watcherConfig{
		sleepingHealthMessage: "vCluster sleeping",
		wakingHealthMessage:   "vCluster waking",
	})

	if got := runtime.lastKnownKargoHealth["kargo-app"].Status; got != "Healthy" {
		t.Fatalf("expected cached healthy state to be preserved, got %q", got)
	}
}

func TestLoadWatcherConfigEnablesApplicationHealthPatchingByDefault(t *testing.T) {
	tokenPath := writeWatcherTestToken(t)

	t.Setenv("WATCH_KUBERNETES_API", "http://127.0.0.1")
	t.Setenv("WATCH_TOKEN_PATH", tokenPath)
	t.Setenv("ARGOCD_CLUSTER_SECRET_NAME_TEMPLATE", "loft-{project}-vcluster-{virtualcluster}")
	t.Setenv("WATCH_PATCH_APPLICATION_HEALTH", "")

	cfg, err := loadWatcherConfig()
	if err != nil {
		t.Fatalf("unexpected error loading watcher config: %v", err)
	}
	if !cfg.patchApplicationHealth {
		t.Fatal("expected application health patching to default to enabled")
	}
}

func TestLoadWatcherConfigAllowsDisablingApplicationHealthPatching(t *testing.T) {
	tokenPath := writeWatcherTestToken(t)

	t.Setenv("WATCH_KUBERNETES_API", "http://127.0.0.1")
	t.Setenv("WATCH_TOKEN_PATH", tokenPath)
	t.Setenv("ARGOCD_CLUSTER_SECRET_NAME_TEMPLATE", "loft-{project}-vcluster-{virtualcluster}")
	t.Setenv("WATCH_PATCH_APPLICATION_HEALTH", "false")

	cfg, err := loadWatcherConfig()
	if err != nil {
		t.Fatalf("unexpected error loading watcher config: %v", err)
	}
	if cfg.patchApplicationHealth {
		t.Fatal("expected application health patching to be disabled")
	}
}

func TestLoadWatcherConfigBuildsWakeRequesterWhenConfigured(t *testing.T) {
	tokenPath := writeWatcherTestToken(t)

	t.Setenv("WATCH_KUBERNETES_API", "http://127.0.0.1")
	t.Setenv("WATCH_TOKEN_PATH", tokenPath)
	t.Setenv("ARGOCD_CLUSTER_SECRET_NAME_TEMPLATE", "loft-{project}-vcluster-{virtualcluster}")
	t.Setenv("WATCH_WAKE_UPSTREAM_BASE", "http://127.0.0.1")

	cfg, err := loadWatcherConfig()
	if err != nil {
		t.Fatalf("unexpected error loading watcher config: %v", err)
	}
	if cfg.wakeRequester == nil {
		t.Fatal("expected wake requester to be configured")
	}
	if cfg.wakeRetryInterval != defaultWakeRetryInterval {
		t.Fatalf("expected default wake retry interval %s, got %s", defaultWakeRetryInterval, cfg.wakeRetryInterval)
	}
}

func TestApplicationsByDestinationName(t *testing.T) {
	apps := []application{
		{
			Metadata: metadata{Name: "guestbook-dev"},
			Spec: applicationSpec{
				Destination: applicationDestination{Name: "loft-default-vcluster-pd-dev"},
			},
		},
		{
			Metadata: metadata{Name: "guestbook-pre-prod"},
			Spec: applicationSpec{
				Destination: applicationDestination{Name: "loft-default-vcluster-pre-prod-gate-pre-prod"},
			},
		},
		{
			Metadata: metadata{Name: "guestbook-pre-prod-copy"},
			Spec: applicationSpec{
				Destination: applicationDestination{Name: "loft-default-vcluster-pre-prod-gate-pre-prod"},
			},
		},
	}

	indexed := applicationsByDestinationName(apps)

	if got := len(indexed["loft-default-vcluster-pd-dev"]); got != 1 {
		t.Fatalf("expected 1 app for pd-dev destination, got %d", got)
	}
	if got := len(indexed["loft-default-vcluster-pre-prod-gate-pre-prod"]); got != 2 {
		t.Fatalf("expected 2 apps for pre-prod destination, got %d", got)
	}
}

func TestApplicationRefreshRequestFingerprint(t *testing.T) {
	app := application{
		Metadata: metadata{
			Name:            "guestbook-dev",
			ResourceVersion: "42",
			Annotations: map[string]string{
				argocdClusterRefreshAnnotation: "normal",
			},
		},
	}

	if got := applicationRefreshRequestFingerprint(app); got != "normal@42" {
		t.Fatalf("expected refresh fingerprint normal@42, got %q", got)
	}
}

func TestApplicationsByAuthorizedStage(t *testing.T) {
	apps := []application{
		{
			Metadata: metadata{
				Name: "guestbook-pre-prod",
				Annotations: map[string]string{
					kargoAuthorizedStageAnnotation: "pre-prod-gate:pre-prod",
				},
			},
		},
		{
			Metadata: metadata{
				Name: "guestbook-prod",
				Annotations: map[string]string{
					kargoAuthorizedStageAnnotation: "pre-prod-gate:prod",
				},
			},
		},
		{
			Metadata: metadata{
				Name: "plain-app",
			},
		},
	}

	indexed := applicationsByAuthorizedStage(apps)

	if got := len(indexed["pre-prod-gate/pre-prod"]); got != 1 {
		t.Fatalf("expected 1 app for pre-prod stage, got %d", got)
	}
	if got := len(indexed["pre-prod-gate/prod"]); got != 1 {
		t.Fatalf("expected 1 app for prod stage, got %d", got)
	}
	if _, ok := indexed[""]; ok {
		t.Fatal("did not expect empty stage key to be indexed")
	}
}

func TestClusterSecretNameTemplate(t *testing.T) {
	got := clusterSecretName("loft-{project}-vcluster-{virtualcluster}", "demo", "team-a")
	if got != "loft-demo-vcluster-team-a" {
		t.Fatalf("expected templated secret name, got %q", got)
	}
}

func TestKargoWakeTriggersByDestination(t *testing.T) {
	apps := []application{
		{
			Metadata: metadata{
				Name: "guestbook-pre-prod",
				Annotations: map[string]string{
					kargoAuthorizedStageAnnotation: "pre-prod-gate:pre-prod",
				},
			},
			Spec: applicationSpec{
				Destination: applicationDestination{Name: "loft-default-vcluster-pre-prod-gate-pre-prod"},
			},
		},
	}

	promotions := []promotion{
		{
			Metadata: metadata{
				Name:      "pre-prod.abc123",
				Namespace: "pre-prod-gate",
			},
			Spec: promotionSpec{
				Stage: "pre-prod",
				Steps: []promotionStep{{Uses: "argocd-update"}},
			},
			Status: promotionStatus{Phase: "Running"},
		},
	}

	triggers := kargoWakeTriggersByDestination(apps, promotions)
	trigger, ok := triggers["loft-default-vcluster-pre-prod-gate-pre-prod"]
	if !ok {
		t.Fatal("expected Kargo wake trigger for destination")
	}
	if len(trigger.Apps) != 1 || trigger.Apps[0].Metadata.Name != "guestbook-pre-prod" {
		t.Fatalf("unexpected trigger apps: %#v", trigger.Apps)
	}
	if len(trigger.PromotionNames) != 1 || trigger.PromotionNames[0] != "pre-prod.abc123" {
		t.Fatalf("unexpected promotion names: %#v", trigger.PromotionNames)
	}
	if trigger.Fingerprint == "" {
		t.Fatal("expected non-empty trigger fingerprint")
	}
}

func TestKargoWakeTriggersIgnoreTerminalAndNonArgoPromotions(t *testing.T) {
	apps := []application{
		{
			Metadata: metadata{
				Name: "guestbook-pre-prod",
				Annotations: map[string]string{
					kargoAuthorizedStageAnnotation: "pre-prod-gate:pre-prod",
				},
			},
			Spec: applicationSpec{
				Destination: applicationDestination{Name: "loft-default-vcluster-pre-prod-gate-pre-prod"},
			},
		},
	}

	promotions := []promotion{
		{
			Metadata: metadata{Name: "pre-prod.done", Namespace: "pre-prod-gate"},
			Spec: promotionSpec{
				Stage: "pre-prod",
				Steps: []promotionStep{{Uses: "argocd-update"}},
			},
			Status: promotionStatus{Phase: "Succeeded"},
		},
		{
			Metadata: metadata{Name: "pre-prod.non-argocd", Namespace: "pre-prod-gate"},
			Spec: promotionSpec{
				Stage: "pre-prod",
				Steps: []promotionStep{{Uses: "git-clone"}},
			},
			Status: promotionStatus{Phase: "Running"},
		},
	}

	if triggers := kargoWakeTriggersByDestination(apps, promotions); len(triggers) != 0 {
		t.Fatalf("expected no wake triggers, got %#v", triggers)
	}
}

func TestReconcileVCITriggersWakeOncePerObservedSyncIntent(t *testing.T) {
	const secretName = "loft-demo-vcluster-team-a"

	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("expected GET request to API server, got %s", r.Method)
		}
		if !strings.HasSuffix(r.URL.Path, "/secrets/"+secretName) {
			t.Fatalf("unexpected API path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"metadata":{"annotations":{"argocd.argoproj.io/skip-reconcile":"true"}}}`))
	}))
	defer apiServer.Close()

	wakeCalls := 0
	wakeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("expected POST wake request, got %s", r.Method)
		}
		if r.URL.Path != "/kubernetes/project/demo/virtualcluster/team-a" {
			t.Fatalf("unexpected wake path %q", r.URL.Path)
		}
		wakeCalls++
		w.WriteHeader(http.StatusAccepted)
	}))
	defer wakeServer.Close()

	cfg := watcherConfig{
		api: &kubernetesAPI{
			client:      apiServer.Client(),
			apiBase:     apiServer.URL,
			bearerToken: "token",
		},
		wakeRequester: &wakeRequester{
			client:           wakeServer.Client(),
			baseURL:          wakeServer.URL,
			acceptedStatuses: parseStatusSet("502,504"),
		},
		wakeRetryInterval:            time.Hour,
		argocdClusterSecretNamespace: "argocd",
		clusterSecretNameTemplate:    "loft-{project}-vcluster-{virtualcluster}",
		projectNamespacePrefixes:     []string{"p-", "loft-p-"},
	}
	runtime := newWatcherRuntime()
	vci := virtualClusterInstance{
		Metadata: metadata{
			Name:      "team-a",
			Namespace: "p-demo",
			Annotations: map[string]string{
				sleepingSinceAnnotation: "1711800000",
			},
		},
	}
	appsByDestination := map[string][]application{
		secretName: {
			{
				Metadata: metadata{Name: "guestbook-pre-prod"},
				Operation: &applicationOperation{
					Sync: json.RawMessage(`{"revision":"abc123"}`),
				},
			},
		},
	}

	if err := reconcileVCI(context.Background(), &cfg, runtime, vci, appsByDestination, nil); err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}
	if wakeCalls != 1 {
		t.Fatalf("expected one wake call after new sync intent, got %d", wakeCalls)
	}

	if err := reconcileVCI(context.Background(), &cfg, runtime, vci, appsByDestination, nil); err != nil {
		t.Fatalf("unexpected reconcile error on second pass: %v", err)
	}
	if wakeCalls != 1 {
		t.Fatalf("expected wake call to be deduplicated, got %d calls", wakeCalls)
	}
}

func TestReconcileVCITriggersWakeOnNewRefreshRequest(t *testing.T) {
	const secretName = "loft-demo-vcluster-team-a"

	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("expected GET request to API server, got %s", r.Method)
		}
		if !strings.HasSuffix(r.URL.Path, "/secrets/"+secretName) {
			t.Fatalf("unexpected API path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"metadata":{"annotations":{"argocd.argoproj.io/skip-reconcile":"true"}}}`))
	}))
	defer apiServer.Close()

	wakeCalls := 0
	wakeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("expected POST wake request, got %s", r.Method)
		}
		if r.URL.Path != "/kubernetes/project/demo/virtualcluster/team-a" {
			t.Fatalf("unexpected wake path %q", r.URL.Path)
		}
		wakeCalls++
		w.WriteHeader(http.StatusAccepted)
	}))
	defer wakeServer.Close()

	cfg := watcherConfig{
		api: &kubernetesAPI{
			client:      apiServer.Client(),
			apiBase:     apiServer.URL,
			bearerToken: "token",
		},
		wakeRequester: &wakeRequester{
			client:           wakeServer.Client(),
			baseURL:          wakeServer.URL,
			acceptedStatuses: parseStatusSet("502,504"),
		},
		wakeRetryInterval:            time.Hour,
		argocdClusterSecretNamespace: "argocd",
		clusterSecretNameTemplate:    "loft-{project}-vcluster-{virtualcluster}",
		projectNamespacePrefixes:     []string{"p-", "loft-p-"},
	}
	runtime := newWatcherRuntime()
	vci := virtualClusterInstance{
		Metadata: metadata{
			Name:      "team-a",
			Namespace: "p-demo",
			Annotations: map[string]string{
				sleepingSinceAnnotation: "1711800000",
			},
		},
	}
	appsByDestination := map[string][]application{
		secretName: {
			{
				Metadata: metadata{
					Name:            "guestbook-pre-prod",
					ResourceVersion: "2",
					Annotations: map[string]string{
						argocdClusterRefreshAnnotation: "normal",
					},
				},
			},
		},
	}

	if err := reconcileVCI(context.Background(), &cfg, runtime, vci, appsByDestination, nil); err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}
	if wakeCalls != 1 {
		t.Fatalf("expected one wake call after new refresh request, got %d", wakeCalls)
	}

	if err := reconcileVCI(context.Background(), &cfg, runtime, vci, appsByDestination, nil); err != nil {
		t.Fatalf("unexpected reconcile error on second pass: %v", err)
	}
	if wakeCalls != 1 {
		t.Fatalf("expected refresh-triggered wake to be deduplicated, got %d calls", wakeCalls)
	}

	appsByDestination[secretName][0].Metadata.ResourceVersion = "3"
	if err := reconcileVCI(context.Background(), &cfg, runtime, vci, appsByDestination, nil); err != nil {
		t.Fatalf("unexpected reconcile error on refreshed pass: %v", err)
	}
	if wakeCalls != 2 {
		t.Fatalf("expected a second wake call after a new refresh request, got %d", wakeCalls)
	}
}

func TestReconcileVCIHardRefreshesReadyAppsOnlyOncePerReadyTransition(t *testing.T) {
	const secretName = "loft-demo-vcluster-team-a"

	var patched []string
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/secrets/"+secretName):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"metadata":{"annotations":{}}}`))
		case r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/applications/guestbook-ready"):
			patched = append(patched, r.URL.Path)
			w.WriteHeader(http.StatusOK)
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer apiServer.Close()

	cfg := watcherConfig{
		api: &kubernetesAPI{
			client:      apiServer.Client(),
			apiBase:     apiServer.URL,
			bearerToken: "token",
		},
		argocdApplicationNamespace:   "argocd",
		argocdClusterSecretNamespace: "argocd",
		clusterSecretNameTemplate:    "loft-{project}-vcluster-{virtualcluster}",
		projectNamespacePrefixes:     []string{"p-", "loft-p-"},
		patchApplicationHealth:       true,
		sleepingHealthMessage:        "vCluster sleeping",
		wakingHealthMessage:          "vCluster waking",
	}
	runtime := newWatcherRuntime()
	vci := virtualClusterInstance{
		Metadata: metadata{
			Name:      "team-a",
			Namespace: "p-demo",
		},
		Status: virtualClusterStatus{
			Phase: "Ready",
			Conditions: []condition{
				{Type: virtualClusterOnlineConditionType, Status: "True"},
			},
		},
	}
	appsByDestination := map[string][]application{
		secretName: {
			{
				Metadata: metadata{Name: "guestbook-ready"},
				Status: applicationStatus{
					Health: healthStatus{
						Status:  "Healthy",
						Message: "vCluster sleeping",
					},
				},
			},
		},
	}

	if err := reconcileVCI(context.Background(), &cfg, runtime, vci, appsByDestination, nil); err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}
	if len(patched) != 1 {
		t.Fatalf("expected one hard refresh patch on first ready reconcile, got %d", len(patched))
	}

	if err := reconcileVCI(context.Background(), &cfg, runtime, vci, appsByDestination, nil); err != nil {
		t.Fatalf("unexpected reconcile error on second pass: %v", err)
	}
	if len(patched) != 1 {
		t.Fatalf("expected hard refresh to be deduplicated while still ready, got %d patches", len(patched))
	}

	vci.Status = virtualClusterStatus{}
	if err := reconcileVCI(context.Background(), &cfg, runtime, vci, appsByDestination, nil); err != nil {
		t.Fatalf("unexpected reconcile error in unknown-state pass: %v", err)
	}

	vci.Status = virtualClusterStatus{
		Phase: "Ready",
		Conditions: []condition{
			{Type: virtualClusterOnlineConditionType, Status: "True"},
		},
	}
	if err := reconcileVCI(context.Background(), &cfg, runtime, vci, appsByDestination, nil); err != nil {
		t.Fatalf("unexpected reconcile error after re-entering ready: %v", err)
	}
	if len(patched) != 2 {
		t.Fatalf("expected another hard refresh after a new ready transition, got %d patches", len(patched))
	}
}

func TestReconcileVCIRepausesIdleReadyClusterDespiteStaleRefreshAnnotation(t *testing.T) {
	const secretName = "loft-demo-vcluster-team-a"

	var secretPatches int
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/secrets/"+secretName):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"metadata":{"annotations":{}}}`))
		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/secrets/"+secretName):
			secretPatches++
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPatch && strings.Contains(r.URL.Path, "/applications/guestbook-ready"):
			w.WriteHeader(http.StatusOK)
		default:
			t.Fatalf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer apiServer.Close()

	cfg := watcherConfig{
		api: &kubernetesAPI{
			client:      apiServer.Client(),
			apiBase:     apiServer.URL,
			bearerToken: "token",
		},
		argocdApplicationNamespace:   "argocd",
		argocdClusterSecretNamespace: "argocd",
		clusterSecretNameTemplate:    "loft-{project}-vcluster-{virtualcluster}",
		projectNamespacePrefixes:     []string{"p-", "loft-p-"},
		patchApplicationHealth:       true,
		sleepingHealthMessage:        "vCluster sleeping",
		wakingHealthMessage:          "vCluster waking",
	}
	runtime := newWatcherRuntime()
	vci := virtualClusterInstance{
		Metadata: metadata{
			Name:      "team-a",
			Namespace: "p-demo",
		},
		Status: virtualClusterStatus{
			Phase: "Ready",
			Conditions: []condition{
				{Type: virtualClusterOnlineConditionType, Status: "True"},
			},
		},
	}
	appsByDestination := map[string][]application{
		secretName: {
			{
				Metadata: metadata{
					Name:            "guestbook-ready",
					ResourceVersion: "42",
					Annotations: map[string]string{
						argocdClusterRefreshAnnotation: "normal",
					},
				},
				Status: applicationStatus{
					Health: healthStatus{
						Status:  "Healthy",
						Message: "",
					},
				},
			},
		},
	}

	if err := reconcileVCI(context.Background(), &cfg, runtime, vci, appsByDestination, nil); err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}
	if secretPatches != 1 {
		t.Fatalf("expected one secret patch to re-pause idle ready cluster, got %d", secretPatches)
	}
}

func TestReconcileVCIDoesNotRetryWakeFromStaleRefreshAnnotationAfterCooldown(t *testing.T) {
	const secretName = "loft-demo-vcluster-team-a"

	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("expected GET request to API server, got %s", r.Method)
		}
		if !strings.HasSuffix(r.URL.Path, "/secrets/"+secretName) {
			t.Fatalf("unexpected API path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"metadata":{"annotations":{"argocd.argoproj.io/skip-reconcile":"true"}}}`))
	}))
	defer apiServer.Close()

	wakeCalls := 0
	wakeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wakeCalls++
		w.WriteHeader(http.StatusAccepted)
	}))
	defer wakeServer.Close()

	cfg := watcherConfig{
		api: &kubernetesAPI{
			client:      apiServer.Client(),
			apiBase:     apiServer.URL,
			bearerToken: "token",
		},
		wakeRequester: &wakeRequester{
			client:           wakeServer.Client(),
			baseURL:          wakeServer.URL,
			acceptedStatuses: parseStatusSet("502,504"),
		},
		wakeRetryInterval:            time.Second,
		argocdClusterSecretNamespace: "argocd",
		clusterSecretNameTemplate:    "loft-{project}-vcluster-{virtualcluster}",
		projectNamespacePrefixes:     []string{"p-", "loft-p-"},
	}
	runtime := newWatcherRuntime()
	vci := virtualClusterInstance{
		Metadata: metadata{
			Name:      "team-a",
			Namespace: "p-demo",
			Annotations: map[string]string{
				sleepingSinceAnnotation: "1711800000",
			},
		},
	}
	appsByDestination := map[string][]application{
		secretName: {
			{
				Metadata: metadata{
					Name:            "guestbook-pre-prod",
					ResourceVersion: "2",
					Annotations: map[string]string{
						argocdClusterRefreshAnnotation: "normal",
					},
				},
			},
		},
	}

	if err := reconcileVCI(context.Background(), &cfg, runtime, vci, appsByDestination, nil); err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}
	if wakeCalls != 1 {
		t.Fatalf("expected one wake call after new refresh request, got %d", wakeCalls)
	}

	runtime.lastWakeAttempt[secretName] = time.Now().Add(-2 * time.Second)
	if err := reconcileVCI(context.Background(), &cfg, runtime, vci, appsByDestination, nil); err != nil {
		t.Fatalf("unexpected reconcile error on stale refresh pass: %v", err)
	}
	if wakeCalls != 1 {
		t.Fatalf("expected no retry wake from stale refresh annotation, got %d calls", wakeCalls)
	}
}

func TestReconcileVCIRetriesWakeAfterCooldownWhenSyncIntentPersists(t *testing.T) {
	const secretName = "loft-demo-vcluster-team-a"

	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"metadata":{"annotations":{"argocd.argoproj.io/skip-reconcile":"true"}}}`))
	}))
	defer apiServer.Close()

	wakeCalls := 0
	wakeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wakeCalls++
		w.WriteHeader(http.StatusAccepted)
	}))
	defer wakeServer.Close()

	cfg := watcherConfig{
		api: &kubernetesAPI{
			client:      apiServer.Client(),
			apiBase:     apiServer.URL,
			bearerToken: "token",
		},
		wakeRequester: &wakeRequester{
			client:           wakeServer.Client(),
			baseURL:          wakeServer.URL,
			acceptedStatuses: parseStatusSet("502,504"),
		},
		wakeRetryInterval:            time.Second,
		argocdClusterSecretNamespace: "argocd",
		clusterSecretNameTemplate:    "loft-{project}-vcluster-{virtualcluster}",
		projectNamespacePrefixes:     []string{"p-", "loft-p-"},
	}
	runtime := newWatcherRuntime()
	runtime.observedSyncIntents["guestbook-pre-prod"] = `{"revision":"abc123"}`
	runtime.lastWakeAttempt[secretName] = time.Now().Add(-2 * time.Second)

	vci := virtualClusterInstance{
		Metadata: metadata{
			Name:      "team-a",
			Namespace: "p-demo",
			Annotations: map[string]string{
				sleepingSinceAnnotation: "1711800000",
			},
		},
	}
	appsByDestination := map[string][]application{
		secretName: {
			{
				Metadata: metadata{Name: "guestbook-pre-prod"},
				Operation: &applicationOperation{
					Sync: json.RawMessage(`{"revision":"abc123"}`),
				},
			},
		},
	}

	if err := reconcileVCI(context.Background(), &cfg, runtime, vci, appsByDestination, nil); err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}
	if wakeCalls != 1 {
		t.Fatalf("expected one retry wake call, got %d", wakeCalls)
	}
}

func TestReconcileVCITriggersWakeForNewOutOfSyncRevision(t *testing.T) {
	const secretName = "loft-demo-vcluster-team-a"

	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("expected GET request to API server, got %s", r.Method)
		}
		if !strings.HasSuffix(r.URL.Path, "/secrets/"+secretName) {
			t.Fatalf("unexpected API path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"metadata":{"annotations":{"argocd.argoproj.io/skip-reconcile":"true"}}}`))
	}))
	defer apiServer.Close()

	wakeCalls := 0
	wakeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("expected POST wake request, got %s", r.Method)
		}
		if r.URL.Path != "/kubernetes/project/demo/virtualcluster/team-a" {
			t.Fatalf("unexpected wake path %q", r.URL.Path)
		}
		wakeCalls++
		w.WriteHeader(http.StatusAccepted)
	}))
	defer wakeServer.Close()

	cfg := watcherConfig{
		api: &kubernetesAPI{
			client:      apiServer.Client(),
			apiBase:     apiServer.URL,
			bearerToken: "token",
		},
		wakeRequester: &wakeRequester{
			client:           wakeServer.Client(),
			baseURL:          wakeServer.URL,
			acceptedStatuses: parseStatusSet("502,504"),
		},
		wakeRetryInterval:            time.Hour,
		argocdClusterSecretNamespace: "argocd",
		clusterSecretNameTemplate:    "loft-{project}-vcluster-{virtualcluster}",
		projectNamespacePrefixes:     []string{"p-", "loft-p-"},
	}
	runtime := newWatcherRuntime()
	vci := virtualClusterInstance{
		Metadata: metadata{
			Name:      "team-a",
			Namespace: "p-demo",
			Annotations: map[string]string{
				sleepingSinceAnnotation: "1711800000",
			},
		},
	}
	appsByDestination := map[string][]application{
		secretName: {
			{
				Metadata: metadata{Name: "guestbook-pre-prod"},
				Status: applicationStatus{
					Sync: applicationSync{
						Status:   "OutOfSync",
						Revision: "abc123",
					},
				},
			},
		},
	}

	if err := reconcileVCI(context.Background(), &cfg, runtime, vci, appsByDestination, nil); err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}
	if wakeCalls != 1 {
		t.Fatalf("expected one wake call after new OutOfSync revision, got %d", wakeCalls)
	}

	if err := reconcileVCI(context.Background(), &cfg, runtime, vci, appsByDestination, nil); err != nil {
		t.Fatalf("unexpected reconcile error on second pass: %v", err)
	}
	if wakeCalls != 1 {
		t.Fatalf("expected OutOfSync revision wake to be deduplicated, got %d calls", wakeCalls)
	}
}

func TestReconcileVCIRetriesWakeAfterCooldownWhenOutOfSyncRevisionPersists(t *testing.T) {
	const secretName = "loft-demo-vcluster-team-a"

	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"metadata":{"annotations":{"argocd.argoproj.io/skip-reconcile":"true"}}}`))
	}))
	defer apiServer.Close()

	wakeCalls := 0
	wakeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wakeCalls++
		w.WriteHeader(http.StatusAccepted)
	}))
	defer wakeServer.Close()

	cfg := watcherConfig{
		api: &kubernetesAPI{
			client:      apiServer.Client(),
			apiBase:     apiServer.URL,
			bearerToken: "token",
		},
		wakeRequester: &wakeRequester{
			client:           wakeServer.Client(),
			baseURL:          wakeServer.URL,
			acceptedStatuses: parseStatusSet("502,504"),
		},
		wakeRetryInterval:            time.Second,
		argocdClusterSecretNamespace: "argocd",
		clusterSecretNameTemplate:    "loft-{project}-vcluster-{virtualcluster}",
		projectNamespacePrefixes:     []string{"p-", "loft-p-"},
	}
	runtime := newWatcherRuntime()
	runtime.observedRevisionWakes["guestbook-pre-prod"] = "abc123"
	runtime.lastWakeAttempt[secretName] = time.Now().Add(-2 * time.Second)

	vci := virtualClusterInstance{
		Metadata: metadata{
			Name:      "team-a",
			Namespace: "p-demo",
			Annotations: map[string]string{
				sleepingSinceAnnotation: "1711800000",
			},
		},
	}
	appsByDestination := map[string][]application{
		secretName: {
			{
				Metadata: metadata{Name: "guestbook-pre-prod"},
				Status: applicationStatus{
					Sync: applicationSync{
						Status:   "OutOfSync",
						Revision: "abc123",
					},
				},
			},
		},
	}

	if err := reconcileVCI(context.Background(), &cfg, runtime, vci, appsByDestination, nil); err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}
	if wakeCalls != 1 {
		t.Fatalf("expected one retry wake call for persistent OutOfSync revision, got %d", wakeCalls)
	}
}

func TestReconcileVCIUpdatesVCILastActivityOnWakeWhenEnabled(t *testing.T) {
	const secretName = "loft-demo-vcluster-team-a"
	const vciStatusPath = "/apis/management.loft.sh/v1/namespaces/p-demo/virtualclusterinstances/team-a/status"

	var patchBodies []string
	before := time.Now().Unix()
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/secrets/"+secretName):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"metadata":{"annotations":{"argocd.argoproj.io/skip-reconcile":"true"}}}`))
		case r.Method == http.MethodPatch && r.URL.Path == vciStatusPath:
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("read status patch body: %v", err)
			}
			patchBodies = append(patchBodies, string(body))
			w.WriteHeader(http.StatusOK)
		default:
			t.Fatalf("unexpected API request %s %q", r.Method, r.URL.Path)
		}
	}))
	defer apiServer.Close()

	wakeCalls := 0
	wakeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wakeCalls++
		w.WriteHeader(http.StatusAccepted)
	}))
	defer wakeServer.Close()

	cfg := watcherConfig{
		api: &kubernetesAPI{
			client:      apiServer.Client(),
			apiBase:     apiServer.URL,
			bearerToken: "token",
		},
		wakeRequester: &wakeRequester{
			client:           wakeServer.Client(),
			baseURL:          wakeServer.URL,
			acceptedStatuses: parseStatusSet("502,504"),
		},
		updateVCILastActivityOnWake:  true,
		wakeRetryInterval:            time.Hour,
		argocdClusterSecretNamespace: "argocd",
		clusterSecretNameTemplate:    "loft-{project}-vcluster-{virtualcluster}",
		projectNamespacePrefixes:     []string{"p-", "loft-p-"},
	}
	runtime := newWatcherRuntime()
	vci := virtualClusterInstance{
		Metadata: metadata{
			Name:      "team-a",
			Namespace: "p-demo",
			Annotations: map[string]string{
				sleepingSinceAnnotation: "1711800000",
			},
		},
	}
	appsByDestination := map[string][]application{
		secretName: {
			{
				Metadata: metadata{Name: "guestbook-pre-prod"},
				Operation: &applicationOperation{
					Sync: json.RawMessage(`{"revision":"abc123"}`),
				},
			},
		},
	}

	if err := reconcileVCI(context.Background(), &cfg, runtime, vci, appsByDestination, nil); err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}
	if wakeCalls != 1 {
		t.Fatalf("expected one wake call, got %d", wakeCalls)
	}
	if len(patchBodies) != 1 {
		t.Fatalf("expected one VCI status patch, got %d", len(patchBodies))
	}

	var payload map[string]any
	if err := json.Unmarshal([]byte(patchBodies[0]), &payload); err != nil {
		t.Fatalf("unmarshal status patch: %v", err)
	}
	status, _ := payload["status"].(map[string]any)
	sleepModeConfig, _ := status["sleepModeConfig"].(map[string]any)
	sleepStatus, _ := sleepModeConfig["status"].(map[string]any)
	lastActivity, _ := sleepStatus["lastActivity"].(float64)
	after := time.Now().Unix()
	if int64(lastActivity) < before || int64(lastActivity) > after {
		t.Fatalf("expected lastActivity to be patched to a current timestamp between %d and %d, got %v", before, after, lastActivity)
	}
}

func TestReconcileVCIIgnoresVCILastActivityPatchFailure(t *testing.T) {
	const secretName = "loft-demo-vcluster-team-a"
	const vciStatusPath = "/apis/management.loft.sh/v1/namespaces/p-demo/virtualclusterinstances/team-a/status"

	patchCalls := 0
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/secrets/"+secretName):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"metadata":{"annotations":{"argocd.argoproj.io/skip-reconcile":"true"}}}`))
		case r.Method == http.MethodPatch && r.URL.Path == vciStatusPath:
			patchCalls++
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"forbidden"}`))
		default:
			t.Fatalf("unexpected API request %s %q", r.Method, r.URL.Path)
		}
	}))
	defer apiServer.Close()

	wakeCalls := 0
	wakeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wakeCalls++
		w.WriteHeader(http.StatusAccepted)
	}))
	defer wakeServer.Close()

	cfg := watcherConfig{
		api: &kubernetesAPI{
			client:      apiServer.Client(),
			apiBase:     apiServer.URL,
			bearerToken: "token",
		},
		wakeRequester: &wakeRequester{
			client:           wakeServer.Client(),
			baseURL:          wakeServer.URL,
			acceptedStatuses: parseStatusSet("502,504"),
		},
		updateVCILastActivityOnWake:  true,
		wakeRetryInterval:            time.Hour,
		argocdClusterSecretNamespace: "argocd",
		clusterSecretNameTemplate:    "loft-{project}-vcluster-{virtualcluster}",
		projectNamespacePrefixes:     []string{"p-", "loft-p-"},
	}
	runtime := newWatcherRuntime()
	vci := virtualClusterInstance{
		Metadata: metadata{
			Name:      "team-a",
			Namespace: "p-demo",
			Annotations: map[string]string{
				sleepingSinceAnnotation: "1711800000",
			},
		},
	}
	appsByDestination := map[string][]application{
		secretName: {
			{
				Metadata: metadata{Name: "guestbook-pre-prod"},
				Operation: &applicationOperation{
					Sync: json.RawMessage(`{"revision":"abc123"}`),
				},
			},
		},
	}

	if err := reconcileVCI(context.Background(), &cfg, runtime, vci, appsByDestination, nil); err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}
	if wakeCalls != 1 {
		t.Fatalf("expected one wake call, got %d", wakeCalls)
	}
	if patchCalls != 1 {
		t.Fatalf("expected one best-effort VCI status patch attempt, got %d", patchCalls)
	}
}

func TestReconcileVCITriggersWakeForActiveKargoPromotionBeforeSyncIntent(t *testing.T) {
	const secretName = "loft-demo-vcluster-team-a"

	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"metadata":{"annotations":{"argocd.argoproj.io/skip-reconcile":"true"}}}`))
	}))
	defer apiServer.Close()

	wakeCalls := 0
	wakeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wakeCalls++
		w.WriteHeader(http.StatusAccepted)
	}))
	defer wakeServer.Close()

	cfg := watcherConfig{
		api: &kubernetesAPI{
			client:      apiServer.Client(),
			apiBase:     apiServer.URL,
			bearerToken: "token",
		},
		wakeRequester: &wakeRequester{
			client:           wakeServer.Client(),
			baseURL:          wakeServer.URL,
			acceptedStatuses: parseStatusSet("502,504"),
		},
		wakeRetryInterval:            time.Hour,
		argocdClusterSecretNamespace: "argocd",
		clusterSecretNameTemplate:    "loft-{project}-vcluster-{virtualcluster}",
		projectNamespacePrefixes:     []string{"p-", "loft-p-"},
	}

	runtime := newWatcherRuntime()
	vci := virtualClusterInstance{
		Metadata: metadata{
			Name:      "team-a",
			Namespace: "p-demo",
			Annotations: map[string]string{
				sleepingSinceAnnotation: "1711800000",
			},
		},
	}
	appsByDestination := map[string][]application{
		secretName: {
			{
				Metadata: metadata{
					Name: "guestbook-pre-prod",
					Annotations: map[string]string{
						kargoAuthorizedStageAnnotation: "pre-prod-gate:pre-prod",
					},
				},
			},
		},
	}
	kargoWakeTriggers := map[string]kargoWakeTrigger{
		secretName: {
			Apps: []application{
				{
					Metadata: metadata{Name: "guestbook-pre-prod"},
				},
			},
			PromotionNames: []string{"pre-prod.abc123"},
			Fingerprint:    "pre-prod.abc123",
		},
	}

	if err := reconcileVCI(context.Background(), &cfg, runtime, vci, appsByDestination, kargoWakeTriggers); err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}
	if wakeCalls != 1 {
		t.Fatalf("expected wake call from active Kargo Promotion, got %d", wakeCalls)
	}
	if got := runtime.observedKargoPromotions[secretName]; got != "pre-prod.abc123" {
		t.Fatalf("expected observed Kargo promotion fingerprint to be remembered, got %q", got)
	}
}

func TestListPromotionsOptionalTreatsMissingKargoAsUnsupported(t *testing.T) {
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/apis/kargo.akuity.io/v1alpha1/promotions" {
			t.Fatalf("unexpected API path %q", r.URL.Path)
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"the server could not find the requested resource"}`))
	}))
	defer apiServer.Close()

	cfg := watcherConfig{
		api: &kubernetesAPI{
			client:      apiServer.Client(),
			apiBase:     apiServer.URL,
			bearerToken: "token",
		},
	}
	runtime := newWatcherRuntime()

	promotions, err := listPromotionsOptional(context.Background(), &cfg, runtime)
	if err != nil {
		t.Fatalf("unexpected error listing optional promotions: %v", err)
	}
	if len(promotions) != 0 {
		t.Fatalf("expected no promotions when Kargo is unavailable, got %#v", promotions)
	}
	if !runtime.kargoPromotionsChecked || runtime.kargoPromotionsAvailable {
		t.Fatalf("expected Kargo promotion discovery to be marked unavailable, got checked=%v available=%v", runtime.kargoPromotionsChecked, runtime.kargoPromotionsAvailable)
	}
}

func writeWatcherTestToken(t *testing.T) string {
	t.Helper()

	tokenFile, err := os.CreateTemp(t.TempDir(), "watcher-token-*")
	if err != nil {
		t.Fatalf("create temp token file: %v", err)
	}
	if _, err := tokenFile.WriteString("test-token\n"); err != nil {
		t.Fatalf("write temp token file: %v", err)
	}
	if err := tokenFile.Close(); err != nil {
		t.Fatalf("close temp token file: %v", err)
	}

	return tokenFile.Name()
}
