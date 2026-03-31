package main

import "testing"

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

	apps[0].Status.Health.Message = "manual override"
	if applicationsNeedReadyRefresh(apps, cfg) {
		t.Fatal("did not expect ready refresh for unrelated app health")
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

func TestClusterSecretNameTemplate(t *testing.T) {
	got := clusterSecretName("loft-{project}-vcluster-{virtualcluster}", "demo", "team-a")
	if got != "loft-demo-vcluster-team-a" {
		t.Fatalf("expected templated secret name, got %q", got)
	}
}
