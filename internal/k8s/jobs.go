package k8s

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"

	"github.com/kube-actions-runner/kube-actions-runner/internal/metrics"
)

type RunnerMode string

const (
	RunnerModeStandard     RunnerMode = "standard"      // Non-root, no Docker, no sudo (legacy)
	RunnerModeUserNS       RunnerMode = "userns"        // User namespace isolation, sudo allowed (default)
	RunnerModeDinD         RunnerMode = "dind"          // Alias for dind-rootless (rootless Docker with user namespaces)
	RunnerModeDinDRootless RunnerMode = "dind-rootless" // Rootless Docker with user namespace isolation
)

const (
	DefaultRunnerImage       = "ghcr.io/actions/actions-runner:2.330.0"
	DefaultDinDRootlessImage = "ghcr.io/actions-runner-controller/actions-runner-controller/actions-runner-dind-rootless:ubuntu-22.04"
)

var validRunnerModes = map[RunnerMode]bool{
	RunnerModeStandard:     true,
	RunnerModeUserNS:       true,
	RunnerModeDinD:         true,
	RunnerModeDinDRootless: true,
}

// Both dind and dind-rootless use the rootless image for security
var defaultRunnerImages = map[RunnerMode]string{
	RunnerModeStandard:     DefaultRunnerImage,
	RunnerModeUserNS:       DefaultRunnerImage,
	RunnerModeDinD:         DefaultDinDRootlessImage,
	RunnerModeDinDRootless: DefaultDinDRootlessImage,
}

// archLabelMap maps workflow labels to kubernetes.io/arch values
var archLabelMap = map[string]string{
	"arm64":   "arm64",
	"aarch64": "arm64",
	"amd64":   "amd64",
	"x64":     "amd64",
	"x86_64":  "amd64",
}

// osLabelMap maps workflow labels to kubernetes.io/os values
var osLabelMap = map[string]string{
	"linux":   "linux",
	"windows": "windows",
}

func ValidRunnerModes() []string {
	modes := make([]string, 0, len(validRunnerModes))
	for mode := range validRunnerModes {
		modes = append(modes, string(mode))
	}
	return modes
}

func IsValidRunnerMode(mode string) bool {
	return validRunnerModes[RunnerMode(mode)]
}

func DefaultImageForMode(mode string) string {
	return defaultRunnerImages[RunnerMode(mode)]
}

type Client struct {
	clientset kubernetes.Interface
	namespace string
}

func NewClient(namespace string) (*Client, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get in-cluster config: %w", err)
	}

	// Increase rate limits to handle burst webhook traffic
	// Default is QPS=5, Burst=10 which is too low for 30+ concurrent webhooks
	config.QPS = 50
	config.Burst = 100

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create clientset: %w", err)
	}

	return &Client{
		clientset: clientset,
		namespace: namespace,
	}, nil
}

func NewClientWithConfig(config *rest.Config, namespace string) (*Client, error) {
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create clientset: %w", err)
	}

	return &Client{
		clientset: clientset,
		namespace: namespace,
	}, nil
}

func NewClientWithClientset(clientset kubernetes.Interface, namespace string) *Client {
	return &Client{
		clientset: clientset,
		namespace: namespace,
	}
}

func (c *Client) JobExists(ctx context.Context, name string) (bool, error) {
	_, err := c.clientset.BatchV1().Jobs(c.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("failed to check job existence: %w", err)
	}
	return true, nil
}

type RunnerJobConfig struct {
	Name        string
	JITConfig   string
	Owner       string
	Repo        string
	WorkflowID  int64
	Labels      []string
	RunnerMode  RunnerMode
	RunnerImage string
	DindImage   string
	TTLSeconds  int32
}

func (c RunnerJobConfig) ttlSeconds() int32 {
	if c.TTLSeconds == 0 {
		return 300
	}
	return c.TTLSeconds
}

func ptr[T any](v T) *T {
	return &v
}

func commonVolumes() []corev1.Volume {
	return []corev1.Volume{
		{Name: "work", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	}
}

func commonVolumeMounts() []corev1.VolumeMount {
	return []corev1.VolumeMount{
		{Name: "work", MountPath: "/home/runner/_work"},
		{Name: "tmp", MountPath: "/tmp"},
	}
}

// buildNodeSelector detects architecture and OS labels from workflow labels
// and returns appropriate nodeSelector values for Kubernetes scheduling
func buildNodeSelector(labels []string) map[string]string {
	nodeSelector := make(map[string]string)

	for _, label := range labels {
		lowerLabel := strings.ToLower(label)

		// Check for architecture labels
		if arch, ok := archLabelMap[lowerLabel]; ok {
			nodeSelector["kubernetes.io/arch"] = arch
		}

		// Check for OS labels
		if os, ok := osLabelMap[lowerLabel]; ok {
			nodeSelector["kubernetes.io/os"] = os
		}
	}

	// Return nil if no selectors were found to avoid empty map in PodSpec
	if len(nodeSelector) == 0 {
		return nil
	}

	return nodeSelector
}

// buildTopologySpreadConstraints returns constraints to spread runner pods across nodes
// This prevents all pods from landing on the same node during burst scheduling
func buildTopologySpreadConstraints() []corev1.TopologySpreadConstraint {
	return []corev1.TopologySpreadConstraint{
		{
			MaxSkew:           2,
			TopologyKey:       "kubernetes.io/hostname",
			WhenUnsatisfiable: corev1.ScheduleAnyway,
			LabelSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": "github-runner",
				},
			},
		},
	}
}

// CreateRunnerJobResult contains the result of creating a runner job
type CreateRunnerJobResult struct {
	// ActualMode is the runner mode that was actually used (may differ from configured mode)
	ActualMode RunnerMode
}

func (c *Client) CreateRunnerJob(ctx context.Context, config RunnerJobConfig) (CreateRunnerJobResult, error) {
	// Determine the actual mode that will be used
	actualMode := DetermineActualMode(config.RunnerMode, config.Labels)

	secretName := fmt.Sprintf("runner-jit-%s", config.Name)
	jobIDStr := fmt.Sprintf("%d", config.WorkflowID)

	// Create the secret first (without OwnerReference) so the pod can mount it immediately
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: c.namespace,
			Labels: map[string]string{
				"app":        "github-runner",
				"owner":      config.Owner,
				"repo":       config.Repo,
				"job-id":     jobIDStr,
				"managed-by": "kube-actions-runner",
			},
		},
		Type: corev1.SecretTypeOpaque,
		StringData: map[string]string{
			"jitconfig": config.JITConfig,
		},
	}

	_, err := c.clientset.CoreV1().Secrets(c.namespace).Create(ctx, secret, metav1.CreateOptions{})
	if err != nil {
		if errors.IsAlreadyExists(err) {
			// Secret exists from a previous attempt - delete and recreate with new JIT config
			// This is critical: old JIT configs have expired registrations
			if err := c.clientset.CoreV1().Secrets(c.namespace).Delete(ctx, secretName, metav1.DeleteOptions{}); err != nil {
				return CreateRunnerJobResult{}, fmt.Errorf("failed to delete old secret: %w", err)
			}
			if _, err := c.clientset.CoreV1().Secrets(c.namespace).Create(ctx, secret, metav1.CreateOptions{}); err != nil {
				return CreateRunnerJobResult{}, fmt.Errorf("failed to recreate secret: %w", err)
			}
		} else {
			return CreateRunnerJobResult{}, fmt.Errorf("failed to create secret: %w", err)
		}
	}

	// Use the actual mode for building the pod spec
	configWithActualMode := config
	configWithActualMode.RunnerMode = actualMode
	podSpec := c.buildPodSpec(configWithActualMode, secretName)

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      config.Name,
			Namespace: c.namespace,
			Labels: map[string]string{
				"app":                         "github-runner",
				"app.kubernetes.io/component": "runner",
				"owner":                       config.Owner,
				"repo":                        config.Repo,
				"job-id":                      jobIDStr,
				"runner-mode":                 string(actualMode),
				"managed-by":                  "kube-actions-runner",
			},
		},
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: ptr(config.ttlSeconds()),
			BackoffLimit:            ptr(int32(0)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app":                         "github-runner",
						"app.kubernetes.io/component": "runner",
						"owner":                       config.Owner,
						"repo":                        config.Repo,
						"job-id":                      jobIDStr,
						"runner-mode":                 string(actualMode),
						"managed-by":                  "kube-actions-runner",
					},
				},
				Spec: podSpec,
			},
		},
	}

	// Create the job
	createdJob, err := c.clientset.BatchV1().Jobs(c.namespace).Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		if errors.IsAlreadyExists(err) {
			// Job already exists - get it to retrieve its UID for the secret patch
			createdJob, err = c.clientset.BatchV1().Jobs(c.namespace).Get(ctx, config.Name, metav1.GetOptions{})
			if err != nil {
				return CreateRunnerJobResult{}, fmt.Errorf("failed to get existing job: %w", err)
			}
		} else {
			return CreateRunnerJobResult{}, fmt.Errorf("failed to create job: %w", err)
		}
	}

	// Patch the secret to add OwnerReference for garbage collection
	// This ensures the secret is deleted when the job is deleted by TTL
	secretPatch := []byte(fmt.Sprintf(`{"metadata":{"ownerReferences":[{"apiVersion":"batch/v1","kind":"Job","name":"%s","uid":"%s"}]}}`,
		createdJob.Name, createdJob.UID))
	_, err = c.clientset.CoreV1().Secrets(c.namespace).Patch(ctx, secretName, types.StrategicMergePatchType, secretPatch, metav1.PatchOptions{})
	if err != nil {
		// Log but don't fail - the secret will be cleaned up by the orphan cleanup routine
		// This is a non-critical operation
	}

	return CreateRunnerJobResult{ActualMode: actualMode}, nil
}

// hasDockerLabel checks if the "docker" label is present in the workflow labels
func hasDockerLabel(labels []string) bool {
	for _, label := range labels {
		if strings.ToLower(label) == "docker" {
			return true
		}
	}
	return false
}

// DetermineActualMode returns the mode that will actually be used for a job,
// taking into account the docker label requirement for dind modes
func DetermineActualMode(configuredMode RunnerMode, labels []string) RunnerMode {
	if configuredMode == "" {
		return RunnerModeUserNS
	}

	// Only use docker modes (dind/dind-rootless) if "docker" label is present
	// Fall back to userns mode which provides user namespace isolation
	if (configuredMode == RunnerModeDinD || configuredMode == RunnerModeDinDRootless) && !hasDockerLabel(labels) {
		return RunnerModeUserNS
	}

	return configuredMode
}

func (c *Client) buildPodSpec(config RunnerJobConfig, secretName string) corev1.PodSpec {
	// Mode should already be determined by DetermineActualMode before calling this
	mode := config.RunnerMode
	if mode == "" {
		mode = RunnerModeUserNS
	}

	switch mode {
	case RunnerModeUserNS:
		return c.buildUserNSPodSpec(config, secretName)
	case RunnerModeDinD, RunnerModeDinDRootless:
		// Both dind and dind-rootless use rootless Docker with user namespaces
		return c.buildDinDRootlessPodSpec(config, secretName)
	default:
		return c.buildStandardPodSpec(config, secretName)
	}
}

// Standard mode: non-root, no Docker, minimal privileges
func (c *Client) buildStandardPodSpec(config RunnerJobConfig, secretName string) corev1.PodSpec {
	image := config.RunnerImage
	if image == "" {
		image = DefaultRunnerImage
	}

	return corev1.PodSpec{
		RestartPolicy:             corev1.RestartPolicyNever,
		NodeSelector:              buildNodeSelector(config.Labels),
		TopologySpreadConstraints: buildTopologySpreadConstraints(),
		SecurityContext: &corev1.PodSecurityContext{
			RunAsNonRoot: ptr(true),
			RunAsUser:    ptr(int64(1000)),
			SeccompProfile: &corev1.SeccompProfile{
				Type: corev1.SeccompProfileTypeRuntimeDefault,
			},
		},
		Containers: []corev1.Container{
			{
				Name:  "runner",
				Image: image,
				Command: []string{"/bin/sh", "-c"},
				Args:    []string{"./run.sh --jitconfig \"$RUNNER_JITCONFIG\""},
				Env: []corev1.EnvVar{
					{
						Name: "RUNNER_JITCONFIG",
						ValueFrom: &corev1.EnvVarSource{
							SecretKeyRef: &corev1.SecretKeySelector{
								LocalObjectReference: corev1.LocalObjectReference{
									Name: secretName,
								},
								Key: "jitconfig",
							},
						},
					},
				},
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: ptr(false),
					Capabilities: &corev1.Capabilities{
						Drop: []corev1.Capability{"ALL"},
					},
				},
				VolumeMounts: commonVolumeMounts(),
			},
		},
		Volumes: commonVolumes(),
	}
}

// UserNS mode: hostUsers=false provides user namespace isolation
// Runs as user 1000 (non-root) but allows privilege escalation for sudo
// User namespaces ensure even root in container is unprivileged on host
func (c *Client) buildUserNSPodSpec(config RunnerJobConfig, secretName string) corev1.PodSpec {
	image := config.RunnerImage
	if image == "" {
		image = DefaultRunnerImage
	}

	return corev1.PodSpec{
		RestartPolicy:             corev1.RestartPolicyNever,
		NodeSelector:              buildNodeSelector(config.Labels),
		TopologySpreadConstraints: buildTopologySpreadConstraints(),
		HostUsers:                 ptr(false),
		SecurityContext: &corev1.PodSecurityContext{
			RunAsNonRoot: ptr(true),
			RunAsUser:    ptr(int64(1000)),
			SeccompProfile: &corev1.SeccompProfile{
				Type: corev1.SeccompProfileTypeRuntimeDefault,
			},
		},
		Containers: []corev1.Container{
			{
				Name:  "runner",
				Image: image,
				Command: []string{"/bin/sh", "-c"},
				Args:    []string{"./run.sh --jitconfig \"$RUNNER_JITCONFIG\""},
				Env: []corev1.EnvVar{
					{
						Name: "RUNNER_JITCONFIG",
						ValueFrom: &corev1.EnvVarSource{
							SecretKeyRef: &corev1.SecretKeySelector{
								LocalObjectReference: corev1.LocalObjectReference{
									Name: secretName,
								},
								Key: "jitconfig",
							},
						},
					},
				},
				// No AllowPrivilegeEscalation: false - allow sudo to work
				// User namespaces provide the real security boundary
				VolumeMounts: commonVolumeMounts(),
			},
		},
		Volumes: commonVolumes(),
	}
}

// DinD mode with user namespace isolation:
// - DinD sidecar runs as root (privileged) to provide Docker
// - Runner runs as non-root user with hostUsers=false for user namespace isolation
// - They communicate via a shared Docker socket
func (c *Client) buildDinDRootlessPodSpec(config RunnerJobConfig, secretName string) corev1.PodSpec {
	runnerImage := config.RunnerImage
	if runnerImage == "" {
		runnerImage = DefaultRunnerImage
	}

	dindImage := config.DindImage
	if dindImage == "" {
		dindImage = "docker:dind"
	}

	return corev1.PodSpec{
		RestartPolicy:             corev1.RestartPolicyNever,
		NodeSelector:              buildNodeSelector(config.Labels),
		TopologySpreadConstraints: buildTopologySpreadConstraints(),
		HostUsers:                 ptr(false), // User namespace isolation for the runner
		SecurityContext: &corev1.PodSecurityContext{
			SeccompProfile: &corev1.SeccompProfile{
				Type: corev1.SeccompProfileTypeRuntimeDefault,
			},
		},
		// DinD runs as a native sidecar (init container with restartPolicy=Always)
		InitContainers: []corev1.Container{
			{
				Name:          "dind",
				Image:         dindImage,
				RestartPolicy: ptr(corev1.ContainerRestartPolicyAlways),
				Env: []corev1.EnvVar{
					{Name: "DOCKER_TLS_CERTDIR", Value: ""}, // Disable TLS for local socket
				},
				SecurityContext: &corev1.SecurityContext{
					Privileged: ptr(true),
				},
				VolumeMounts: []corev1.VolumeMount{
					{Name: "docker-socket", MountPath: "/var/run"},
					{Name: "docker-data", MountPath: "/var/lib/docker"},
				},
			},
		},
		Containers: []corev1.Container{
			{
				Name:    "runner",
				Image:   runnerImage,
				Command: []string{"/bin/sh", "-c"},
				Args:    []string{"./run.sh --jitconfig \"$RUNNER_JITCONFIG\""},
				Env: []corev1.EnvVar{
					{
						Name: "RUNNER_JITCONFIG",
						ValueFrom: &corev1.EnvVarSource{
							SecretKeyRef: &corev1.SecretKeySelector{
								LocalObjectReference: corev1.LocalObjectReference{
									Name: secretName,
								},
								Key: "jitconfig",
							},
						},
					},
					{Name: "DOCKER_HOST", Value: "unix:///var/run/docker.sock"},
				},
				// Runner runs as non-root, user namespace provides isolation
				VolumeMounts: append(commonVolumeMounts(),
					corev1.VolumeMount{Name: "docker-socket", MountPath: "/var/run"},
				),
			},
		},
		Volumes: append(commonVolumes(),
			corev1.Volume{Name: "docker-socket", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			corev1.Volume{Name: "docker-data", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		),
	}
}

// GetAvailableArchitectures lists all nodes in the cluster and returns a map of
// available architectures based on the kubernetes.io/arch label.
// Only includes nodes that are in Ready condition.
func (c *Client) GetAvailableArchitectures(ctx context.Context) (map[string]bool, error) {
	nodes, err := c.clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("failed to list nodes: %w", err)
	}

	archs := make(map[string]bool)
	for _, node := range nodes.Items {
		// Check if node is Ready
		if !isNodeReady(&node) {
			continue
		}

		// Extract architecture label
		if arch, ok := node.Labels["kubernetes.io/arch"]; ok {
			archs[arch] = true
		}
	}

	return archs, nil
}

// isNodeReady checks if a node has the Ready condition set to True
func isNodeReady(node *corev1.Node) bool {
	for _, condition := range node.Status.Conditions {
		if condition.Type == corev1.NodeReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

// HasNodesForArchitecture checks if the cluster has any Ready nodes
// with the specified architecture.
func (c *Client) HasNodesForArchitecture(ctx context.Context, arch string) (bool, error) {
	archs, err := c.GetAvailableArchitectures(ctx)
	if err != nil {
		return false, err
	}
	return archs[arch], nil
}

// GetRequiredArchFromLabels extracts the Kubernetes architecture value from
// workflow labels. Returns empty string if no architecture label is found.
func GetRequiredArchFromLabels(labels []string) string {
	for _, label := range labels {
		lowerLabel := strings.ToLower(label)
		if arch, ok := archLabelMap[lowerLabel]; ok {
			return arch
		}
	}
	return ""
}

const (
	// WebhookSecretName is the name of the K8s secret that stores the webhook secret
	WebhookSecretName = "kube-actions-runner-webhook-secret"
	// WebhookSecretKey is the key within the secret that holds the webhook secret value
	WebhookSecretKey = "webhook-secret"
)

// GetWebhookSecret retrieves the persisted webhook secret from Kubernetes.
// Returns empty string and nil error if the secret doesn't exist yet.
func (c *Client) GetWebhookSecret(ctx context.Context) (string, error) {
	secret, err := c.clientset.CoreV1().Secrets(c.namespace).Get(ctx, WebhookSecretName, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return "", nil
		}
		return "", fmt.Errorf("failed to get webhook secret: %w", err)
	}

	if data, ok := secret.Data[WebhookSecretKey]; ok {
		return string(data), nil
	}
	return "", nil
}

// SaveWebhookSecret persists the webhook secret to Kubernetes.
// Creates the secret if it doesn't exist, updates it if it does.
func (c *Client) SaveWebhookSecret(ctx context.Context, webhookSecret string) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      WebhookSecretName,
			Namespace: c.namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":       "kube-actions-runner",
				"app.kubernetes.io/component":  "webhook-secret",
				"app.kubernetes.io/managed-by": "kube-actions-runner",
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			WebhookSecretKey: []byte(webhookSecret),
		},
	}

	// Try to create first
	_, err := c.clientset.CoreV1().Secrets(c.namespace).Create(ctx, secret, metav1.CreateOptions{})
	if err != nil {
		if errors.IsAlreadyExists(err) {
			// Update existing secret
			_, err = c.clientset.CoreV1().Secrets(c.namespace).Update(ctx, secret, metav1.UpdateOptions{})
			if err != nil {
				return fmt.Errorf("failed to update webhook secret: %w", err)
			}
			return nil
		}
		return fmt.Errorf("failed to create webhook secret: %w", err)
	}
	return nil
}

// ListRunnerPods lists all runner pods in the namespace
func (c *Client) ListRunnerPods(ctx context.Context) ([]corev1.Pod, error) {
	pods, err := c.clientset.CoreV1().Pods(c.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app=github-runner",
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list runner pods: %w", err)
	}
	return pods.Items, nil
}

// CleanupOrphanedSecrets deletes runner JIT secrets that don't have an OwnerReference.
// These are orphaned secrets from before OwnerReferences were added.
// Returns the number of secrets deleted.
func (c *Client) CleanupOrphanedSecrets(ctx context.Context) (int, error) {
	// List all runner JIT secrets
	secrets, err := c.clientset.CoreV1().Secrets(c.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app=github-runner,managed-by=kube-actions-runner",
	})
	if err != nil {
		return 0, fmt.Errorf("failed to list runner secrets: %w", err)
	}

	deleted := 0
	for _, secret := range secrets.Items {
		// Skip secrets that have an OwnerReference (they'll be cleaned up automatically)
		if len(secret.OwnerReferences) > 0 {
			continue
		}

		// Skip the webhook secret
		if secret.Name == WebhookSecretName {
			continue
		}

		// This is an orphaned secret - delete it
		err := c.clientset.CoreV1().Secrets(c.namespace).Delete(ctx, secret.Name, metav1.DeleteOptions{})
		if err != nil && !errors.IsNotFound(err) {
			// Log but continue - don't fail the whole cleanup for one secret
			continue
		}
		deleted++
	}

	return deleted, nil
}

// RunnerJobState contains the state of a runner job for reconciler decision making
type RunnerJobState struct {
	Exists    bool
	CreatedAt time.Time
}

// ListRunnerJobStates returns job states for all runner jobs in the namespace.
// This is more reliable than checking pods since jobs are created before pods.
func (c *Client) ListRunnerJobStates(ctx context.Context) (map[int64]RunnerJobState, error) {
	jobs, err := c.clientset.BatchV1().Jobs(c.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app=github-runner",
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list runner jobs: %w", err)
	}

	result := make(map[int64]RunnerJobState)
	for _, job := range jobs.Items {
		if jobIDStr, ok := job.Labels["job-id"]; ok {
			var id int64
			fmt.Sscanf(jobIDStr, "%d", &id)
			if id > 0 {
				result[id] = RunnerJobState{
					Exists:    true,
					CreatedAt: job.CreationTimestamp.Time,
				}
			}
		}
	}
	return result, nil
}

// IsRunnerPodStale checks if a runner pod is stale (waiting for jobs) by examining its logs.
// A stale runner will have "Listening for Jobs" as one of its recent log lines.
// Returns: isStale (true if waiting for jobs), error
func (c *Client) IsRunnerPodStale(ctx context.Context, podName string) (bool, error) {
	// Get the last few lines of logs from the runner container
	tailLines := int64(10)
	req := c.clientset.CoreV1().Pods(c.namespace).GetLogs(podName, &corev1.PodLogOptions{
		Container: "runner",
		TailLines: &tailLines,
	})

	stream, err := req.Stream(ctx)
	if err != nil {
		return false, fmt.Errorf("failed to get pod logs: %w", err)
	}
	defer stream.Close()

	// Read the logs and check for "Listening for Jobs"
	scanner := bufio.NewScanner(stream)
	var lastLines []string
	for scanner.Scan() {
		lastLines = append(lastLines, scanner.Text())
	}

	if err := scanner.Err(); err != nil && err != io.EOF {
		return false, fmt.Errorf("failed to read pod logs: %w", err)
	}

	// Check if "Listening for Jobs" appears in recent logs
	// This indicates the runner is idle and waiting for work
	for _, line := range lastLines {
		if strings.Contains(line, "Listening for Jobs") {
			return true, nil
		}
	}

	// If we don't see "Listening for Jobs", the runner is likely processing a job
	return false, nil
}

// CleanupAttemptedAnnotation is set on jobs where cleanup was attempted but the runner
// was not found in GitHub. This prevents retry storms when the runner is already gone.
const CleanupAttemptedAnnotation = "kube-actions-runner.github.io/cleanup-attempted"

// RunnerJobInfo contains information about a runner job for cleanup purposes
type RunnerJobInfo struct {
	Name             string
	PodName          string          // Name of the pod for log access
	Owner            string
	Repo             string
	JobID            int64
	RunnerName       string
	Phase            corev1.PodPhase
	IsActive         bool // True if job has active pods and hasn't completed/failed
	CleanupAttempted bool // True if cleanup was already attempted
}

// ListActiveRunnerJobs lists all active runner jobs with their metadata
// Returns jobs that are currently running (have active pods and haven't completed)
func (c *Client) ListActiveRunnerJobs(ctx context.Context) ([]RunnerJobInfo, error) {
	jobs, err := c.clientset.BatchV1().Jobs(c.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app=github-runner",
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list runner jobs: %w", err)
	}

	var result []RunnerJobInfo
	for _, job := range jobs.Items {
		// Skip completed or failed jobs
		if job.Status.Succeeded > 0 || job.Status.Failed > 0 {
			continue
		}

		// Parse job ID from labels
		var jobID int64
		if jobIDStr, ok := job.Labels["job-id"]; ok {
			fmt.Sscanf(jobIDStr, "%d", &jobID)
		}

		// Get pod phase and name for the job's pod
		podPhase := corev1.PodUnknown
		podName := ""
		pods, err := c.clientset.CoreV1().Pods(c.namespace).List(ctx, metav1.ListOptions{
			LabelSelector: fmt.Sprintf("job-name=%s", job.Name),
		})
		if err == nil && len(pods.Items) > 0 {
			podPhase = pods.Items[0].Status.Phase
			podName = pods.Items[0].Name
		}

		// Check if cleanup was already attempted
		_, cleanupAttempted := job.Annotations[CleanupAttemptedAnnotation]

		result = append(result, RunnerJobInfo{
			Name:             job.Name,
			PodName:          podName,
			Owner:            job.Labels["owner"],
			Repo:             job.Labels["repo"],
			JobID:            jobID,
			RunnerName:       job.Name, // Runner name matches job name
			Phase:            podPhase,
			IsActive:         job.Status.Active > 0 || (job.Status.Succeeded == 0 && job.Status.Failed == 0),
			CleanupAttempted: cleanupAttempted,
		})
	}

	return result, nil
}

// MarkCleanupAttempted adds an annotation to a job indicating that cleanup was attempted.
// This prevents retry storms when a runner is already gone from GitHub.
func (c *Client) MarkCleanupAttempted(ctx context.Context, jobName string) error {
	job, err := c.clientset.BatchV1().Jobs(c.namespace).Get(ctx, jobName, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			// Job already gone, nothing to mark
			return nil
		}
		return fmt.Errorf("failed to get job %s: %w", jobName, err)
	}

	// Initialize annotations map if nil
	if job.Annotations == nil {
		job.Annotations = make(map[string]string)
	}

	// Add the cleanup-attempted annotation with current timestamp
	job.Annotations[CleanupAttemptedAnnotation] = time.Now().UTC().Format(time.RFC3339)

	_, err = c.clientset.BatchV1().Jobs(c.namespace).Update(ctx, job, metav1.UpdateOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			// Job was deleted between get and update, that's fine
			return nil
		}
		return fmt.Errorf("failed to update job %s with cleanup annotation: %w", jobName, err)
	}

	return nil
}

// CountActiveRunnerJobs counts the number of active runner jobs
func (c *Client) CountActiveRunnerJobs(ctx context.Context) (int, error) {
	jobs, err := c.clientset.BatchV1().Jobs(c.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app=github-runner",
	})
	if err != nil {
		return 0, fmt.Errorf("failed to list runner jobs: %w", err)
	}

	activeCount := 0
	for _, job := range jobs.Items {
		// A job is active if it has active pods and hasn't completed or failed
		if job.Status.Active > 0 && job.Status.Succeeded == 0 && job.Status.Failed == 0 {
			activeCount++
		}
	}
	return activeCount, nil
}

// SyncActiveJobsMetric queries K8s and sets the RunnerJobsActive gauge to the actual count
func (c *Client) SyncActiveJobsMetric(ctx context.Context) error {
	count, err := c.CountActiveRunnerJobs(ctx)
	if err != nil {
		return err
	}
	metrics.RunnerJobsActive.Set(float64(count))
	return nil
}

// StartJobWatcher starts an informer to watch for job completions and update metrics.
// Returns a stop function to cancel the watcher.
func (c *Client) StartJobWatcher(ctx context.Context) (func(), error) {
	// Create a shared informer factory scoped to our namespace
	factory := informers.NewSharedInformerFactoryWithOptions(
		c.clientset,
		30*time.Second, // Resync period
		informers.WithNamespace(c.namespace),
	)

	jobInformer := factory.Batch().V1().Jobs().Informer()

	_, err := jobInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldJob, ok1 := oldObj.(*batchv1.Job)
			newJob, ok2 := newObj.(*batchv1.Job)
			if !ok1 || !ok2 {
				return
			}

			// Only process jobs with our label
			if newJob.Labels["app"] != "github-runner" {
				return
			}

			// Check if job just completed (was active, now succeeded or failed)
			wasActive := oldJob.Status.Active > 0 || (oldJob.Status.Succeeded == 0 && oldJob.Status.Failed == 0 && oldJob.Status.Active == 0 && oldJob.Status.StartTime != nil)
			isCompleted := newJob.Status.Succeeded > 0 || newJob.Status.Failed > 0

			if wasActive && isCompleted {
				metrics.RunnerJobsActive.Dec()
			}
		},
		DeleteFunc: func(obj interface{}) {
			job, ok := obj.(*batchv1.Job)
			if !ok {
				// Handle DeletedFinalStateUnknown
				if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
					job, ok = tombstone.Obj.(*batchv1.Job)
					if !ok {
						return
					}
				} else {
					return
				}
			}

			// Only process jobs with our label
			if job.Labels["app"] != "github-runner" {
				return
			}

			// If job was deleted while still active, decrement
			if job.Status.Active > 0 || (job.Status.Succeeded == 0 && job.Status.Failed == 0) {
				metrics.RunnerJobsActive.Dec()
			}
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to add event handler: %w", err)
	}

	// Create stop channel
	stopCh := make(chan struct{})

	// Start the informer
	go factory.Start(stopCh)

	// Wait for cache sync
	if !cache.WaitForCacheSync(ctx.Done(), jobInformer.HasSynced) {
		close(stopCh)
		return nil, fmt.Errorf("failed to sync job informer cache")
	}

	return func() { close(stopCh) }, nil
}
