package k8s

import (
	"context"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestBuildPodSpec_StandardMode(t *testing.T) {
	client := &Client{namespace: "test-ns"}
	config := RunnerJobConfig{
		Name:       "runner-12345",
		JITConfig:  "encoded-jit-config",
		Owner:      "test-owner",
		Repo:       "test-repo",
		WorkflowID: 12345,
		Labels:     []string{"self-hosted", "linux"},
		RunnerMode: RunnerModeStandard,
	}

	podSpec := client.buildPodSpec(config, "secret-name")

	if podSpec.SecurityContext == nil {
		t.Fatal("expected pod security context to be set")
	}
	if podSpec.SecurityContext.RunAsUser == nil || *podSpec.SecurityContext.RunAsUser != 1000 {
		t.Error("expected RunAsUser to be 1000")
	}
	if podSpec.SecurityContext.RunAsNonRoot == nil || !*podSpec.SecurityContext.RunAsNonRoot {
		t.Error("expected RunAsNonRoot to be true")
	}

	if len(podSpec.Containers) != 1 {
		t.Fatalf("expected 1 container, got %d", len(podSpec.Containers))
	}

	container := podSpec.Containers[0]
	if container.Name != "runner" {
		t.Errorf("expected container name 'runner', got %q", container.Name)
	}

	if container.SecurityContext == nil {
		t.Fatal("expected container security context")
	}
	if container.SecurityContext.Privileged != nil && *container.SecurityContext.Privileged {
		t.Error("standard mode should not be privileged")
	}
	if container.SecurityContext.AllowPrivilegeEscalation == nil || *container.SecurityContext.AllowPrivilegeEscalation {
		t.Error("expected AllowPrivilegeEscalation to be false")
	}

	if container.Image != DefaultRunnerImage {
		t.Errorf("expected default image %q, got %q", DefaultRunnerImage, container.Image)
	}
}

func TestBuildPodSpec_StandardModeCustomImage(t *testing.T) {
	client := &Client{namespace: "test-ns"}
	customImage := "my-custom-runner:v1"
	config := RunnerJobConfig{
		Name:        "runner-12345",
		RunnerMode:  RunnerModeStandard,
		RunnerImage: customImage,
	}

	podSpec := client.buildPodSpec(config, "secret-name")

	if podSpec.Containers[0].Image != customImage {
		t.Errorf("expected custom image %q, got %q", customImage, podSpec.Containers[0].Image)
	}
}

func TestBuildPodSpec_UserNSMode(t *testing.T) {
	client := &Client{namespace: "test-ns"}
	config := RunnerJobConfig{
		Name:       "runner-12345",
		RunnerMode: RunnerModeUserNS,
	}

	podSpec := client.buildPodSpec(config, "secret-name")

	if podSpec.HostUsers == nil || *podSpec.HostUsers {
		t.Error("expected HostUsers to be false for userns mode")
	}

	if podSpec.SecurityContext == nil {
		t.Fatal("expected pod security context")
	}
	if podSpec.SecurityContext.RunAsUser == nil || *podSpec.SecurityContext.RunAsUser != 1000 {
		t.Error("expected RunAsUser to be 1000 for userns mode")
	}

	if len(podSpec.Containers) != 1 {
		t.Fatalf("expected 1 container, got %d", len(podSpec.Containers))
	}

	container := podSpec.Containers[0]
	if container.SecurityContext != nil && container.SecurityContext.Privileged != nil && *container.SecurityContext.Privileged {
		t.Error("userns mode should not be privileged")
	}
}

func TestBuildPodSpec_DinDMode(t *testing.T) {
	client := &Client{namespace: "test-ns"}
	config := RunnerJobConfig{
		Name:       "runner-12345",
		RunnerMode: RunnerModeDinD,
		Labels:     []string{"self-hosted", "docker"},
	}

	podSpec := client.buildPodSpec(config, "secret-name")

	// Runner should be in Containers, dind should be in InitContainers (native sidecar)
	if len(podSpec.Containers) != 1 {
		t.Fatalf("expected 1 container (runner), got %d", len(podSpec.Containers))
	}
	if len(podSpec.InitContainers) != 1 {
		t.Fatalf("expected 1 init container (dind sidecar), got %d", len(podSpec.InitContainers))
	}

	runnerContainer := &podSpec.Containers[0]
	dindContainer := &podSpec.InitContainers[0]

	if runnerContainer.Name != "runner" {
		t.Errorf("expected runner container name 'runner', got %q", runnerContainer.Name)
	}
	if dindContainer.Name != "dind" {
		t.Errorf("expected dind container name 'dind', got %q", dindContainer.Name)
	}

	// Verify dind is a native sidecar (restartPolicy: Always)
	if dindContainer.RestartPolicy == nil {
		t.Fatal("expected dind container to have restartPolicy set")
	}
	if *dindContainer.RestartPolicy != corev1.ContainerRestartPolicyAlways {
		t.Errorf("expected dind restartPolicy=Always, got %v", *dindContainer.RestartPolicy)
	}

	// Verify dind has startup probe
	if dindContainer.StartupProbe == nil {
		t.Fatal("expected dind container to have startupProbe")
	}

	if dindContainer.SecurityContext == nil {
		t.Fatal("expected dind security context")
	}
	if dindContainer.SecurityContext.Privileged == nil || !*dindContainer.SecurityContext.Privileged {
		t.Error("dind sidecar should be privileged")
	}

	hasDockerHost := false
	for _, env := range runnerContainer.Env {
		if env.Name == "DOCKER_HOST" && env.Value == "tcp://localhost:2376" {
			hasDockerHost = true
			break
		}
	}
	if !hasDockerHost {
		t.Error("runner should have DOCKER_HOST env var pointing to dind sidecar")
	}

	// DinD mode cannot use user namespaces - dockerd needs real privileged access
	if podSpec.HostUsers != nil && !*podSpec.HostUsers {
		t.Error("dind mode should not use user namespace isolation (dockerd needs real privileges)")
	}
}

func TestBuildPodSpec_DinDModeCustomImages(t *testing.T) {
	client := &Client{namespace: "test-ns"}
	customRunner := "my-runner:v1"
	customDind := "my-dind:v1"
	config := RunnerJobConfig{
		Name:        "runner-12345",
		RunnerMode:  RunnerModeDinD,
		RunnerImage: customRunner,
		DindImage:   customDind,
		Labels:      []string{"self-hosted", "docker"},
	}

	podSpec := client.buildPodSpec(config, "secret-name")

	// Check runner container image
	if len(podSpec.Containers) != 1 || podSpec.Containers[0].Name != "runner" {
		t.Fatal("expected single runner container")
	}
	if podSpec.Containers[0].Image != customRunner {
		t.Errorf("expected runner image %q, got %q", customRunner, podSpec.Containers[0].Image)
	}

	// Check dind init container (native sidecar) image
	if len(podSpec.InitContainers) != 1 || podSpec.InitContainers[0].Name != "dind" {
		t.Fatal("expected single dind init container")
	}
	if podSpec.InitContainers[0].Image != customDind {
		t.Errorf("expected dind image %q, got %q", customDind, podSpec.InitContainers[0].Image)
	}
}

func TestBuildPodSpec_DinDRootlessMode(t *testing.T) {
	client := &Client{namespace: "test-ns"}
	config := RunnerJobConfig{
		Name:       "runner-12345",
		RunnerMode: RunnerModeDinDRootless,
		Labels:     []string{"self-hosted", "docker"},
	}

	podSpec := client.buildPodSpec(config, "secret-name")

	if podSpec.HostUsers == nil || *podSpec.HostUsers {
		t.Error("expected HostUsers to be false for dind-rootless mode")
	}

	if len(podSpec.Containers) != 1 {
		t.Fatalf("expected 1 container, got %d", len(podSpec.Containers))
	}

	container := podSpec.Containers[0]

	if container.SecurityContext == nil {
		t.Fatal("expected container security context")
	}
	if container.SecurityContext.Privileged == nil || !*container.SecurityContext.Privileged {
		t.Error("dind-rootless container should be privileged (within user namespace)")
	}

	hasDockerHost := false
	for _, env := range container.Env {
		if env.Name == "DOCKER_HOST" && env.Value == "unix:///run/user/1000/docker.sock" {
			hasDockerHost = true
			break
		}
	}
	if !hasDockerHost {
		t.Error("dind-rootless should have DOCKER_HOST env var for rootless socket")
	}

	if container.Image != DefaultDinDRootlessImage {
		t.Errorf("expected default rootless image %q, got %q", DefaultDinDRootlessImage, container.Image)
	}
}

func TestBuildPodSpec_DefaultsToUserNS(t *testing.T) {
	client := &Client{namespace: "test-ns"}
	config := RunnerJobConfig{
		Name:       "runner-12345",
		RunnerMode: RunnerMode(""),
	}

	podSpec := client.buildPodSpec(config, "secret-name")

	// Default mode is now userns which uses user namespaces
	if podSpec.HostUsers == nil || *podSpec.HostUsers {
		t.Error("expected hostUsers=false for default userns mode")
	}
	if podSpec.SecurityContext == nil {
		t.Fatal("expected pod security context")
	}
	// userns mode runs as user 1000 (non-root) but allows sudo via privilege escalation
	if podSpec.SecurityContext.RunAsUser == nil || *podSpec.SecurityContext.RunAsUser != 1000 {
		t.Error("expected RunAsUser to be 1000 (userns mode default)")
	}
	if len(podSpec.Containers) != 1 {
		t.Error("expected single container for default mode")
	}
}

func TestDetermineActualMode_DinDFallsBackWithoutDockerLabel(t *testing.T) {
	// When dind mode is configured but no "docker" label, should fall back to userns
	tests := []struct {
		mode   RunnerMode
		labels []string
	}{
		{RunnerModeDinD, []string{"self-hosted", "linux"}},
		{RunnerModeDinD, nil},
		{RunnerModeDinDRootless, []string{"self-hosted", "linux"}},
		{RunnerModeDinDRootless, nil},
	}

	for _, tt := range tests {
		t.Run(string(tt.mode), func(t *testing.T) {
			actualMode := DetermineActualMode(tt.mode, tt.labels)

			// Should fall back to userns mode (user namespace isolation)
			if actualMode != RunnerModeUserNS {
				t.Errorf("expected mode to fall back to userns, got %s", actualMode)
			}
		})
	}
}

func TestDetermineActualMode_DinDUsedWithDockerLabel(t *testing.T) {
	// When dind mode is configured with "docker" label, should use dind
	tests := []struct {
		mode   RunnerMode
		labels []string
	}{
		{RunnerModeDinD, []string{"self-hosted", "linux", "docker"}},
		{RunnerModeDinD, []string{"docker"}},
		{RunnerModeDinDRootless, []string{"self-hosted", "Docker"}}, // case insensitive
		{RunnerModeDinDRootless, []string{"DOCKER"}},
	}

	for _, tt := range tests {
		t.Run(string(tt.mode), func(t *testing.T) {
			actualMode := DetermineActualMode(tt.mode, tt.labels)

			// Should keep the configured mode
			if actualMode != tt.mode {
				t.Errorf("expected mode to stay as %s, got %s", tt.mode, actualMode)
			}
		})
	}
}

func TestBuildPodSpec_StandardModeHasCorrectConfig(t *testing.T) {
	client := &Client{namespace: "test-ns"}

	config := RunnerJobConfig{
		Name:       "runner-12345",
		RunnerMode: RunnerModeStandard,
		Labels:     []string{"self-hosted", "linux"},
	}

	podSpec := client.buildPodSpec(config, "secret-name")

	// Should have single container
	if len(podSpec.Containers) != 1 {
		t.Errorf("expected 1 container, got %d", len(podSpec.Containers))
	}

	container := podSpec.Containers[0]
	if container.Name != "runner" {
		t.Errorf("expected container name 'runner', got %q", container.Name)
	}

	// Standard mode should have non-root user
	if podSpec.SecurityContext == nil || podSpec.SecurityContext.RunAsUser == nil || *podSpec.SecurityContext.RunAsUser != 1000 {
		t.Error("expected RunAsUser to be 1000 (standard mode)")
	}

	// Standard mode should not be privileged
	if container.SecurityContext != nil && container.SecurityContext.Privileged != nil && *container.SecurityContext.Privileged {
		t.Error("standard mode should not be privileged")
	}
}

func TestBuildPodSpec_HostUsersSettings(t *testing.T) {
	client := &Client{namespace: "test-ns"}

	tests := []struct {
		mode             RunnerMode
		labels           []string
		expectHostUsers  *bool // nil means not set, otherwise expected value
		description      string
	}{
		{RunnerModeStandard, nil, nil, "standard mode should not set hostUsers"},
		{RunnerModeUserNS, nil, ptr(false), "userns mode should set hostUsers=false"},
		{RunnerModeDinD, []string{"docker"}, nil, "dind mode needs real privileged access, no user namespaces"},
		{RunnerModeDinDRootless, []string{"docker"}, ptr(false), "dind-rootless mode should set hostUsers=false"},
	}

	for _, tt := range tests {
		t.Run(string(tt.mode), func(t *testing.T) {
			config := RunnerJobConfig{
				Name:       "runner-12345",
				RunnerMode: tt.mode,
				Labels:     tt.labels,
			}

			podSpec := client.buildPodSpec(config, "secret-name")

			if tt.expectHostUsers == nil {
				if podSpec.HostUsers != nil && !*podSpec.HostUsers {
					t.Errorf("%s: expected hostUsers to be unset or true", tt.description)
				}
			} else {
				if podSpec.HostUsers == nil {
					t.Errorf("%s: expected hostUsers to be set", tt.description)
				} else if *podSpec.HostUsers != *tt.expectHostUsers {
					t.Errorf("%s: got hostUsers=%v", tt.description, *podSpec.HostUsers)
				}
			}
		})
	}
}

func TestBuildPodSpec_PrivilegedSettings(t *testing.T) {
	client := &Client{namespace: "test-ns"}

	tests := []struct {
		mode             RunnerMode
		labels           []string
		expectPrivileged bool
		containerName    string
		isInitContainer  bool // true if container is in InitContainers (native sidecar)
		description      string
	}{
		{RunnerModeStandard, nil, false, "runner", false, "standard runner should not be privileged"},
		{RunnerModeUserNS, nil, false, "runner", false, "userns runner should not be privileged"},
		{RunnerModeDinD, []string{"docker"}, true, "dind", true, "dind sidecar should be privileged"},
		{RunnerModeDinDRootless, []string{"docker"}, true, "runner", false, "dind-rootless runner should be privileged"},
	}

	for _, tt := range tests {
		t.Run(string(tt.mode), func(t *testing.T) {
			config := RunnerJobConfig{
				Name:       "runner-12345",
				RunnerMode: tt.mode,
				Labels:     tt.labels,
			}

			podSpec := client.buildPodSpec(config, "secret-name")

			var targetContainer *corev1.Container
			containers := podSpec.Containers
			if tt.isInitContainer {
				containers = podSpec.InitContainers
			}
			for i := range containers {
				if containers[i].Name == tt.containerName {
					targetContainer = &containers[i]
					break
				}
			}

			if targetContainer == nil {
				t.Fatalf("container %q not found", tt.containerName)
			}

			isPrivileged := targetContainer.SecurityContext != nil &&
				targetContainer.SecurityContext.Privileged != nil &&
				*targetContainer.SecurityContext.Privileged

			if isPrivileged != tt.expectPrivileged {
				t.Errorf("%s: got privileged=%v, want %v", tt.description, isPrivileged, tt.expectPrivileged)
			}
		})
	}
}

func TestBuildPodSpec_JITConfigEnvVar(t *testing.T) {
	client := &Client{namespace: "test-ns"}
	secretName := "runner-jit-runner-12345"

	tests := []struct {
		mode   RunnerMode
		labels []string
	}{
		{RunnerModeStandard, nil},
		{RunnerModeUserNS, nil},
		{RunnerModeDinD, []string{"docker"}},
		{RunnerModeDinDRootless, []string{"docker"}},
	}

	for _, tt := range tests {
		t.Run(string(tt.mode), func(t *testing.T) {
			config := RunnerJobConfig{
				Name:       "runner-12345",
				RunnerMode: tt.mode,
				Labels:     tt.labels,
			}

			podSpec := client.buildPodSpec(config, secretName)

			var runnerContainer *corev1.Container
			for i := range podSpec.Containers {
				if podSpec.Containers[i].Name == "runner" {
					runnerContainer = &podSpec.Containers[i]
					break
				}
			}

			if runnerContainer == nil {
				t.Fatal("runner container not found")
			}

			hasJitConfig := false
			for _, env := range runnerContainer.Env {
				if env.Name == "RUNNER_JITCONFIG" {
					if env.ValueFrom == nil || env.ValueFrom.SecretKeyRef == nil {
						t.Error("RUNNER_JITCONFIG should be from secret")
					} else if env.ValueFrom.SecretKeyRef.Name != secretName {
						t.Errorf("expected secret name %q, got %q", secretName, env.ValueFrom.SecretKeyRef.Name)
					} else if env.ValueFrom.SecretKeyRef.Key != "jitconfig" {
						t.Errorf("expected secret key 'jitconfig', got %q", env.ValueFrom.SecretKeyRef.Key)
					}
					hasJitConfig = true
					break
				}
			}

			if !hasJitConfig {
				t.Error("runner container should have RUNNER_JITCONFIG env var")
			}
		})
	}
}

func TestBuildPodSpec_RestartPolicyNever(t *testing.T) {
	client := &Client{namespace: "test-ns"}
	tests := []struct {
		mode   RunnerMode
		labels []string
	}{
		{RunnerModeStandard, nil},
		{RunnerModeUserNS, nil},
		{RunnerModeDinD, []string{"docker"}},
		{RunnerModeDinDRootless, []string{"docker"}},
	}

	for _, tt := range tests {
		t.Run(string(tt.mode), func(t *testing.T) {
			config := RunnerJobConfig{
				Name:       "runner-12345",
				RunnerMode: tt.mode,
				Labels:     tt.labels,
			}

			podSpec := client.buildPodSpec(config, "secret-name")

			if podSpec.RestartPolicy != corev1.RestartPolicyNever {
				t.Errorf("expected RestartPolicy=Never, got %v", podSpec.RestartPolicy)
			}
		})
	}
}

func TestJobNamingWithWorkflowID(t *testing.T) {
	workflowID := int64(987654321)
	expectedName := "runner-987654321"

	config := RunnerJobConfig{
		Name:       expectedName,
		WorkflowID: workflowID,
		RunnerMode: RunnerModeStandard,
	}

	if config.Name != expectedName {
		t.Errorf("expected job name %q, got %q", expectedName, config.Name)
	}
}

func TestJobExists_ReturnsTrue_WhenJobExists(t *testing.T) {
	fakeClientset := fake.NewSimpleClientset(&batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "runner-12345",
			Namespace: "test-ns",
		},
	})
	client := NewClientWithClientset(fakeClientset, "test-ns")

	exists, err := client.JobExists(context.Background(), "runner-12345")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !exists {
		t.Error("expected job to exist, but JobExists returned false")
	}
}

func TestJobExists_ReturnsFalse_WhenJobNotFound(t *testing.T) {
	fakeClientset := fake.NewSimpleClientset()
	client := NewClientWithClientset(fakeClientset, "test-ns")

	exists, err := client.JobExists(context.Background(), "nonexistent-job")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if exists {
		t.Error("expected job to not exist, but JobExists returned true")
	}
}

func TestCreateRunnerJob_CreatesSecretAndJob(t *testing.T) {
	fakeClientset := fake.NewSimpleClientset()
	client := NewClientWithClientset(fakeClientset, "test-ns")

	config := RunnerJobConfig{
		Name:       "runner-12345",
		JITConfig:  "encoded-jit-config",
		Owner:      "test-owner",
		Repo:       "test-repo",
		WorkflowID: 12345,
		Labels:     []string{"self-hosted", "linux"},
		RunnerMode: RunnerModeStandard,
	}
	_, err := client.CreateRunnerJob(context.Background(), config)
	if err != nil {
		t.Fatalf("CreateRunnerJob failed: %v", err)
	}

	secret, err := fakeClientset.CoreV1().Secrets("test-ns").Get(context.Background(), "runner-jit-runner-12345", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get secret: %v", err)
	}
	if secret.Name != "runner-jit-runner-12345" {
		t.Errorf("expected secret name 'runner-jit-runner-12345', got %q", secret.Name)
	}
	if secret.Labels["app"] != "github-runner" {
		t.Errorf("expected secret label app='github-runner', got %q", secret.Labels["app"])
	}
	if secret.Labels["owner"] != "test-owner" {
		t.Errorf("expected secret label owner='test-owner', got %q", secret.Labels["owner"])
	}
	if secret.Labels["repo"] != "test-repo" {
		t.Errorf("expected secret label repo='test-repo', got %q", secret.Labels["repo"])
	}
	if secret.Labels["managed-by"] != "kube-actions-runner" {
		t.Errorf("expected secret label managed-by='kube-actions-runner', got %q", secret.Labels["managed-by"])
	}
	if secret.StringData["jitconfig"] != "encoded-jit-config" {
		t.Errorf("expected jitconfig='encoded-jit-config', got %q", secret.StringData["jitconfig"])
	}

	job, err := fakeClientset.BatchV1().Jobs("test-ns").Get(context.Background(), "runner-12345", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get job: %v", err)
	}
	if job.Name != "runner-12345" {
		t.Errorf("expected job name 'runner-12345', got %q", job.Name)
	}
	if job.Labels["app"] != "github-runner" {
		t.Errorf("expected job label app='github-runner', got %q", job.Labels["app"])
	}
	if job.Labels["owner"] != "test-owner" {
		t.Errorf("expected job label owner='test-owner', got %q", job.Labels["owner"])
	}
	if job.Labels["repo"] != "test-repo" {
		t.Errorf("expected job label repo='test-repo', got %q", job.Labels["repo"])
	}
	if job.Labels["runner-mode"] != "standard" {
		t.Errorf("expected job label runner-mode='standard', got %q", job.Labels["runner-mode"])
	}
	if job.Labels["managed-by"] != "kube-actions-runner" {
		t.Errorf("expected job label managed-by='kube-actions-runner', got %q", job.Labels["managed-by"])
	}

	if job.Spec.TTLSecondsAfterFinished == nil || *job.Spec.TTLSecondsAfterFinished != 300 {
		t.Error("expected TTLSecondsAfterFinished to be 300")
	}
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
		t.Error("expected BackoffLimit to be 0")
	}

	podSpec := job.Spec.Template.Spec
	if len(podSpec.Containers) != 1 {
		t.Fatalf("expected 1 container, got %d", len(podSpec.Containers))
	}
	if podSpec.Containers[0].Name != "runner" {
		t.Errorf("expected container name 'runner', got %q", podSpec.Containers[0].Name)
	}
	if podSpec.Containers[0].Image != DefaultRunnerImage {
		t.Errorf("expected image %q, got %q", DefaultRunnerImage, podSpec.Containers[0].Image)
	}

	foundJitConfig := false
	for _, env := range podSpec.Containers[0].Env {
		if env.Name == "RUNNER_JITCONFIG" {
			foundJitConfig = true
			if env.ValueFrom == nil || env.ValueFrom.SecretKeyRef == nil {
				t.Error("expected RUNNER_JITCONFIG to reference a secret")
			} else if env.ValueFrom.SecretKeyRef.Name != "runner-jit-runner-12345" {
				t.Errorf("expected secret reference 'runner-jit-runner-12345', got %q", env.ValueFrom.SecretKeyRef.Name)
			}
			break
		}
	}
	if !foundJitConfig {
		t.Error("expected RUNNER_JITCONFIG env var in container")
	}
}

func TestCreateRunnerJob_CustomTTL(t *testing.T) {
	fakeClientset := fake.NewSimpleClientset()
	client := NewClientWithClientset(fakeClientset, "test-ns")

	config := RunnerJobConfig{
		Name:       "runner-12345",
		JITConfig:  "encoded-jit-config",
		Owner:      "test-owner",
		Repo:       "test-repo",
		WorkflowID: 12345,
		Labels:     []string{"self-hosted", "linux"},
		RunnerMode: RunnerModeStandard,
		TTLSeconds: 600, // Custom TTL
	}
	_, err := client.CreateRunnerJob(context.Background(), config)
	if err != nil {
		t.Fatalf("CreateRunnerJob failed: %v", err)
	}

	job, err := fakeClientset.BatchV1().Jobs("test-ns").Get(context.Background(), "runner-12345", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get job: %v", err)
	}

	if job.Spec.TTLSecondsAfterFinished == nil || *job.Spec.TTLSecondsAfterFinished != 600 {
		t.Errorf("expected TTLSecondsAfterFinished to be 600, got %v", job.Spec.TTLSecondsAfterFinished)
	}
}

func TestTTLSeconds_DefaultValue(t *testing.T) {
	config := RunnerJobConfig{
		TTLSeconds: 0,
	}
	if config.ttlSeconds() != 300 {
		t.Errorf("expected default TTL 300, got %d", config.ttlSeconds())
	}
}

func TestTTLSeconds_CustomValue(t *testing.T) {
	config := RunnerJobConfig{
		TTLSeconds: 600,
	}
	if config.ttlSeconds() != 600 {
		t.Errorf("expected TTL 600, got %d", config.ttlSeconds())
	}
}

func TestCreateRunnerJob_HandlesExistingSecret(t *testing.T) {
	existingSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "runner-jit-runner-12345",
			Namespace: "test-ns",
		},
		Data: map[string][]byte{
			"jitconfig": []byte("old-config"),
		},
	}
	fakeClientset := fake.NewSimpleClientset(existingSecret)
	client := NewClientWithClientset(fakeClientset, "test-ns")

	config := RunnerJobConfig{
		Name:       "runner-12345",
		JITConfig:  "new-jit-config",
		Owner:      "test-owner",
		Repo:       "test-repo",
		WorkflowID: 12345,
		Labels:     []string{"self-hosted"},
		RunnerMode: RunnerModeStandard,
	}
	_, err := client.CreateRunnerJob(context.Background(), config)

	if err != nil {
		t.Fatalf("CreateRunnerJob should not error when secret already exists: %v", err)
	}

	job, err := fakeClientset.BatchV1().Jobs("test-ns").Get(context.Background(), "runner-12345", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get job: %v", err)
	}
	if job.Name != "runner-12345" {
		t.Errorf("expected job name 'runner-12345', got %q", job.Name)
	}
}

func TestBuildNodeSelector(t *testing.T) {
	tests := []struct {
		name           string
		labels         []string
		expectedArch   string
		expectedOS     string
		expectNil      bool
	}{
		{
			name:         "arm64 label",
			labels:       []string{"arm64"},
			expectedArch: "arm64",
		},
		{
			name:         "aarch64 label maps to arm64",
			labels:       []string{"aarch64"},
			expectedArch: "arm64",
		},
		{
			name:         "amd64 label",
			labels:       []string{"amd64"},
			expectedArch: "amd64",
		},
		{
			name:         "x64 label maps to amd64",
			labels:       []string{"x64"},
			expectedArch: "amd64",
		},
		{
			name:         "x86_64 label maps to amd64",
			labels:       []string{"x86_64"},
			expectedArch: "amd64",
		},
		{
			name:       "linux label",
			labels:     []string{"linux"},
			expectedOS: "linux",
		},
		{
			name:       "windows label",
			labels:     []string{"windows"},
			expectedOS: "windows",
		},
		{
			name:         "combined labels with arch and os",
			labels:       []string{"self-hosted", "linux", "arm64"},
			expectedArch: "arm64",
			expectedOS:   "linux",
		},
		{
			name:         "case insensitivity - ARM64",
			labels:       []string{"ARM64"},
			expectedArch: "arm64",
		},
		{
			name:       "case insensitivity - Linux",
			labels:     []string{"Linux"},
			expectedOS: "linux",
		},
		{
			name:         "case insensitivity - mixed case",
			labels:       []string{"LINUX", "AArch64"},
			expectedArch: "arm64",
			expectedOS:   "linux",
		},
		{
			name:      "empty labels",
			labels:    []string{},
			expectNil: true,
		},
		{
			name:      "nil labels",
			labels:    nil,
			expectNil: true,
		},
		{
			name:      "labels without arch or os - just self-hosted",
			labels:    []string{"self-hosted"},
			expectNil: true,
		},
		{
			name:      "unknown labels are ignored",
			labels:    []string{"self-hosted", "custom-label", "my-runner"},
			expectNil: true,
		},
		{
			name:         "unknown labels mixed with valid labels",
			labels:       []string{"self-hosted", "custom-label", "arm64", "linux"},
			expectedArch: "arm64",
			expectedOS:   "linux",
		},
		{
			name:         "multiple arch labels - last one wins",
			labels:       []string{"arm64", "amd64"},
			expectedArch: "amd64",
		},
		{
			name:       "multiple os labels - last one wins",
			labels:     []string{"linux", "windows"},
			expectedOS: "windows",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := buildNodeSelector(tt.labels)

			if tt.expectNil {
				if result != nil {
					t.Errorf("expected nil, got %v", result)
				}
				return
			}

			if result == nil {
				t.Fatal("expected non-nil result")
			}

			if tt.expectedArch != "" {
				if arch, ok := result["kubernetes.io/arch"]; !ok {
					t.Error("expected kubernetes.io/arch to be set")
				} else if arch != tt.expectedArch {
					t.Errorf("expected arch %q, got %q", tt.expectedArch, arch)
				}
			} else {
				if _, ok := result["kubernetes.io/arch"]; ok {
					t.Error("expected kubernetes.io/arch to not be set")
				}
			}

			if tt.expectedOS != "" {
				if os, ok := result["kubernetes.io/os"]; !ok {
					t.Error("expected kubernetes.io/os to be set")
				} else if os != tt.expectedOS {
					t.Errorf("expected os %q, got %q", tt.expectedOS, os)
				}
			} else {
				if _, ok := result["kubernetes.io/os"]; ok {
					t.Error("expected kubernetes.io/os to not be set")
				}
			}
		})
	}
}

func TestCreateRunnerJobWithArchLabels(t *testing.T) {
	tests := []struct {
		name         string
		labels       []string
		expectedArch string
		expectedOS   string
		expectNil    bool
	}{
		{
			name:         "arm64 linux labels",
			labels:       []string{"self-hosted", "linux", "arm64"},
			expectedArch: "arm64",
			expectedOS:   "linux",
		},
		{
			name:         "amd64 linux labels",
			labels:       []string{"self-hosted", "linux", "amd64"},
			expectedArch: "amd64",
			expectedOS:   "linux",
		},
		{
			name:         "x64 windows labels",
			labels:       []string{"self-hosted", "windows", "x64"},
			expectedArch: "amd64",
			expectedOS:   "windows",
		},
		{
			name:      "no arch or os labels",
			labels:    []string{"self-hosted"},
			expectNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeClientset := fake.NewSimpleClientset()
			client := NewClientWithClientset(fakeClientset, "test-ns")

			config := RunnerJobConfig{
				Name:       "runner-12345",
				JITConfig:  "encoded-jit-config",
				Owner:      "test-owner",
				Repo:       "test-repo",
				WorkflowID: 12345,
				Labels:     tt.labels,
				RunnerMode: RunnerModeStandard,
			}

			_, err := client.CreateRunnerJob(context.Background(), config)
			if err != nil {
				t.Fatalf("CreateRunnerJob failed: %v", err)
			}

			job, err := fakeClientset.BatchV1().Jobs("test-ns").Get(context.Background(), "runner-12345", metav1.GetOptions{})
			if err != nil {
				t.Fatalf("failed to get job: %v", err)
			}

			nodeSelector := job.Spec.Template.Spec.NodeSelector

			if tt.expectNil {
				if nodeSelector != nil && len(nodeSelector) > 0 {
					t.Errorf("expected nil or empty nodeSelector, got %v", nodeSelector)
				}
				return
			}

			if nodeSelector == nil {
				t.Fatal("expected nodeSelector to be set")
			}

			if tt.expectedArch != "" {
				if arch, ok := nodeSelector["kubernetes.io/arch"]; !ok {
					t.Error("expected kubernetes.io/arch to be set in nodeSelector")
				} else if arch != tt.expectedArch {
					t.Errorf("expected arch %q, got %q", tt.expectedArch, arch)
				}
			}

			if tt.expectedOS != "" {
				if os, ok := nodeSelector["kubernetes.io/os"]; !ok {
					t.Error("expected kubernetes.io/os to be set in nodeSelector")
				} else if os != tt.expectedOS {
					t.Errorf("expected os %q, got %q", tt.expectedOS, os)
				}
			}
		})
	}
}

func TestBuildPodSpec_NodeSelectorAcrossModes(t *testing.T) {
	client := &Client{namespace: "test-ns"}

	tests := []struct {
		mode   RunnerMode
		labels []string
	}{
		{RunnerModeStandard, []string{"self-hosted", "linux", "arm64"}},
		{RunnerModeUserNS, []string{"self-hosted", "linux", "arm64"}},
		{RunnerModeDinD, []string{"self-hosted", "linux", "arm64", "docker"}},
		{RunnerModeDinDRootless, []string{"self-hosted", "linux", "arm64", "docker"}},
	}

	for _, tt := range tests {
		t.Run(string(tt.mode), func(t *testing.T) {
			config := RunnerJobConfig{
				Name:       "runner-12345",
				Labels:     tt.labels,
				RunnerMode: tt.mode,
			}

			podSpec := client.buildPodSpec(config, "secret-name")

			if podSpec.NodeSelector == nil {
				t.Fatal("expected nodeSelector to be set")
			}

			if arch, ok := podSpec.NodeSelector["kubernetes.io/arch"]; !ok {
				t.Error("expected kubernetes.io/arch to be set")
			} else if arch != "arm64" {
				t.Errorf("expected arch 'arm64', got %q", arch)
			}

			if os, ok := podSpec.NodeSelector["kubernetes.io/os"]; !ok {
				t.Error("expected kubernetes.io/os to be set")
			} else if os != "linux" {
				t.Errorf("expected os 'linux', got %q", os)
			}
		})
	}
}

func TestGetRequiredArchFromLabels(t *testing.T) {
	tests := []struct {
		name     string
		labels   []string
		expected string
	}{
		{
			name:     "arm64 label returns arm64",
			labels:   []string{"self-hosted", "linux", "arm64"},
			expected: "arm64",
		},
		{
			name:     "aarch64 label returns arm64",
			labels:   []string{"self-hosted", "linux", "aarch64"},
			expected: "arm64",
		},
		{
			name:     "amd64 label returns amd64",
			labels:   []string{"self-hosted", "linux", "amd64"},
			expected: "amd64",
		},
		{
			name:     "x64 label returns amd64",
			labels:   []string{"self-hosted", "linux", "x64"},
			expected: "amd64",
		},
		{
			name:     "x86_64 label returns amd64",
			labels:   []string{"self-hosted", "linux", "x86_64"},
			expected: "amd64",
		},
		{
			name:     "mixed case ARM64 returns arm64",
			labels:   []string{"self-hosted", "linux", "ARM64"},
			expected: "arm64",
		},
		{
			name:     "labels without arch returns empty string",
			labels:   []string{"self-hosted", "linux"},
			expected: "",
		},
		{
			name:     "empty labels returns empty string",
			labels:   []string{},
			expected: "",
		},
		{
			name:     "nil labels returns empty string",
			labels:   nil,
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := GetRequiredArchFromLabels(tt.labels)
			if result != tt.expected {
				t.Errorf("GetRequiredArchFromLabels(%v) = %q, want %q", tt.labels, result, tt.expected)
			}
		})
	}
}

func TestIsNodeReady(t *testing.T) {
	tests := []struct {
		name     string
		node     *corev1.Node
		expected bool
	}{
		{
			name: "node with Ready=True condition returns true",
			node: &corev1.Node{
				Status: corev1.NodeStatus{
					Conditions: []corev1.NodeCondition{
						{
							Type:   corev1.NodeReady,
							Status: corev1.ConditionTrue,
						},
					},
				},
			},
			expected: true,
		},
		{
			name: "node with Ready=False condition returns false",
			node: &corev1.Node{
				Status: corev1.NodeStatus{
					Conditions: []corev1.NodeCondition{
						{
							Type:   corev1.NodeReady,
							Status: corev1.ConditionFalse,
						},
					},
				},
			},
			expected: false,
		},
		{
			name: "node with Ready=Unknown condition returns false",
			node: &corev1.Node{
				Status: corev1.NodeStatus{
					Conditions: []corev1.NodeCondition{
						{
							Type:   corev1.NodeReady,
							Status: corev1.ConditionUnknown,
						},
					},
				},
			},
			expected: false,
		},
		{
			name: "node with no Ready condition returns false",
			node: &corev1.Node{
				Status: corev1.NodeStatus{
					Conditions: []corev1.NodeCondition{
						{
							Type:   corev1.NodeMemoryPressure,
							Status: corev1.ConditionFalse,
						},
						{
							Type:   corev1.NodeDiskPressure,
							Status: corev1.ConditionFalse,
						},
					},
				},
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isNodeReady(tt.node)
			if result != tt.expected {
				t.Errorf("isNodeReady() = %v, want %v", result, tt.expected)
			}
		})
	}
}
