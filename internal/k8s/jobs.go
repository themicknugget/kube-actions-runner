package k8s

import (
	"context"
	"fmt"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type RunnerMode string

const (
	RunnerModeStandard     RunnerMode = "standard"      // Non-root, no Docker
	RunnerModeUserNS       RunnerMode = "userns"        // User namespace isolation, no Docker
	RunnerModeDinD         RunnerMode = "dind"          // Privileged DinD sidecar (use with caution)
	RunnerModeDinDRootless RunnerMode = "dind-rootless" // Rootless DinD in user namespace (recommended for Docker)
)

const (
	DefaultRunnerImage       = "ghcr.io/actions/actions-runner:2.321.0"
	DefaultDinDRootlessImage = "ghcr.io/actions/actions-runner-dind-rootless:2.321.0"
	DefaultDinDSidecarImage  = "docker:27.4.1-dind"
)

var validRunnerModes = map[RunnerMode]bool{
	RunnerModeStandard:     true,
	RunnerModeUserNS:       true,
	RunnerModeDinD:         true,
	RunnerModeDinDRootless: true,
}

var defaultRunnerImages = map[RunnerMode]string{
	RunnerModeStandard:     DefaultRunnerImage,
	RunnerModeUserNS:       DefaultRunnerImage,
	RunnerModeDinD:         DefaultRunnerImage,
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

func (c *Client) CreateRunnerJob(ctx context.Context, config RunnerJobConfig) error {
	secretName := fmt.Sprintf("runner-jit-%s", config.Name)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: c.namespace,
			Labels: map[string]string{
				"app":        "github-runner",
				"owner":      config.Owner,
				"repo":       config.Repo,
				"managed-by": "kube-actions-runner",
			},
		},
		Type: corev1.SecretTypeOpaque,
		StringData: map[string]string{
			"jitconfig": config.JITConfig,
		},
	}

	_, err := c.clientset.CoreV1().Secrets(c.namespace).Create(ctx, secret, metav1.CreateOptions{})
	if err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("failed to create secret: %w", err)
	}

	podSpec := c.buildPodSpec(config, secretName)

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      config.Name,
			Namespace: c.namespace,
			Labels: map[string]string{
				"app":         "github-runner",
				"owner":       config.Owner,
				"repo":        config.Repo,
				"runner-mode": string(config.RunnerMode),
				"managed-by":  "kube-actions-runner",
			},
		},
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: ptr(config.ttlSeconds()),
			BackoffLimit:            ptr(int32(0)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app":         "github-runner",
						"owner":       config.Owner,
						"repo":        config.Repo,
						"runner-mode": string(config.RunnerMode),
						"managed-by":  "kube-actions-runner",
					},
				},
				Spec: podSpec,
			},
		},
	}

	_, err = c.clientset.BatchV1().Jobs(c.namespace).Create(ctx, job, metav1.CreateOptions{})
	if err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("failed to create job: %w", err)
	}

	return nil
}

func (c *Client) buildPodSpec(config RunnerJobConfig, secretName string) corev1.PodSpec {
	mode := config.RunnerMode
	if mode == "" {
		mode = RunnerModeStandard
	}

	switch mode {
	case RunnerModeUserNS:
		return c.buildUserNSPodSpec(config, secretName)
	case RunnerModeDinD:
		return c.buildDinDPodSpec(config, secretName)
	case RunnerModeDinDRootless:
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
		RestartPolicy: corev1.RestartPolicyNever,
		NodeSelector:  buildNodeSelector(config.Labels),
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
					ReadOnlyRootFilesystem:   ptr(true),
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

// UserNS mode: hostUsers=false maps container root to unprivileged host user
func (c *Client) buildUserNSPodSpec(config RunnerJobConfig, secretName string) corev1.PodSpec {
	image := config.RunnerImage
	if image == "" {
		image = DefaultRunnerImage
	}

	return corev1.PodSpec{
		RestartPolicy: corev1.RestartPolicyNever,
		NodeSelector:  buildNodeSelector(config.Labels),
		HostUsers:     ptr(false),
		SecurityContext: &corev1.PodSecurityContext{
			RunAsUser: ptr(int64(0)),
			SeccompProfile: &corev1.SeccompProfile{
				Type: corev1.SeccompProfileTypeRuntimeDefault,
			},
		},
		Containers: []corev1.Container{
			{
				Name:  "runner",
				Image: image,
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
				VolumeMounts: commonVolumeMounts(),
			},
		},
		Volumes: commonVolumes(),
	}
}

// DinD mode: privileged sidecar - use with caution, has full host access
func (c *Client) buildDinDPodSpec(config RunnerJobConfig, secretName string) corev1.PodSpec {
	image := config.RunnerImage
	if image == "" {
		image = DefaultRunnerImage
	}

	dindImage := config.DindImage
	if dindImage == "" {
		dindImage = DefaultDinDSidecarImage
	}

	return corev1.PodSpec{
		RestartPolicy: corev1.RestartPolicyNever,
		NodeSelector:  buildNodeSelector(config.Labels),
		SecurityContext: &corev1.PodSecurityContext{
			SeccompProfile: &corev1.SeccompProfile{
				Type: corev1.SeccompProfileTypeRuntimeDefault,
			},
		},
		Containers: []corev1.Container{
			{
				Name:  "runner",
				Image: image,
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
					{Name: "DOCKER_HOST", Value: "tcp://localhost:2376"},
					{Name: "DOCKER_TLS_VERIFY", Value: "1"},
					{Name: "DOCKER_CERT_PATH", Value: "/certs/client"},
				},
				VolumeMounts: append(commonVolumeMounts(), corev1.VolumeMount{
					Name:      "docker-certs",
					MountPath: "/certs/client",
					ReadOnly:  true,
				}),
			},
			{
				Name:  "dind",
				Image: dindImage,
				SecurityContext: &corev1.SecurityContext{
					Privileged: ptr(true),
				},
				Env: []corev1.EnvVar{
					{Name: "DOCKER_TLS_CERTDIR", Value: "/certs"},
				},
				VolumeMounts: []corev1.VolumeMount{
					{
						Name:      "docker-certs",
						MountPath: "/certs/client",
					},
					{
						Name:      "dind-storage",
						MountPath: "/var/lib/docker",
					},
				},
			},
		},
		Volumes: append(commonVolumes(),
			corev1.Volume{Name: "docker-certs", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			corev1.Volume{Name: "dind-storage", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		),
	}
}

// DinD-Rootless mode: hostUsers=false + privileged confines privileges to user namespace (recommended)
func (c *Client) buildDinDRootlessPodSpec(config RunnerJobConfig, secretName string) corev1.PodSpec {
	image := config.RunnerImage
	if image == "" {
		image = DefaultDinDRootlessImage
	}

	return corev1.PodSpec{
		RestartPolicy: corev1.RestartPolicyNever,
		NodeSelector:  buildNodeSelector(config.Labels),
		HostUsers:     ptr(false),
		SecurityContext: &corev1.PodSecurityContext{
			SeccompProfile: &corev1.SeccompProfile{
				Type: corev1.SeccompProfileTypeUnconfined,
			},
		},
		Containers: []corev1.Container{
			{
				Name:  "runner",
				Image: image,
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
					{Name: "DOCKER_HOST", Value: "unix:///run/user/1000/docker.sock"},
					{Name: "XDG_RUNTIME_DIR", Value: "/run/user/1000"},
				},
				SecurityContext: &corev1.SecurityContext{
					Privileged: ptr(true),
				},
				VolumeMounts: append(commonVolumeMounts(),
					corev1.VolumeMount{Name: "run-user", MountPath: "/run/user/1000"},
					corev1.VolumeMount{Name: "docker-data", MountPath: "/home/runner/.local/share/docker"},
				),
			},
		},
		Volumes: append(commonVolumes(),
			corev1.Volume{Name: "run-user", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			corev1.Volume{Name: "docker-data", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		),
	}
}
