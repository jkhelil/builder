package strategy

import (
	"fmt"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"

	"github.com/golang/glog"
	"k8s.io/kubernetes/pkg/apis/policy"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kvalidation "k8s.io/apimachinery/pkg/util/validation"

	buildv1 "github.com/openshift/api/build/v1"
	"github.com/openshift/origin/pkg/api/apihelpers"
	"github.com/openshift/origin/pkg/build/buildapihelpers"
	buildutil "github.com/openshift/origin/pkg/build/util"
	"github.com/openshift/origin/pkg/image/apis/image/reference"
)

const (
	// dockerSocketPath is the default path for the Docker socket inside the builder container
	dockerSocketPath      = "/var/run/docker.sock"
	sourceSecretMountPath = "/var/run/secrets/openshift.io/source"

	DockerPushSecretMountPath            = "/var/run/secrets/openshift.io/push"
	DockerPullSecretMountPath            = "/var/run/secrets/openshift.io/pull"
	ConfigMapBuildSourceBaseMountPath    = "/var/run/configs/openshift.io/build"
	ConfigMapBuildSystemConfigsMountPath = "/var/run/configs/openshift.io/build-system"
	ConfigMapCertsMountPath              = "/var/run/configs/openshift.io/certs"
	SecretBuildSourceBaseMountPath       = "/var/run/secrets/openshift.io/build"
	SourceImagePullSecretMountPath       = "/var/run/secrets/openshift.io/source-image"

	// ExtractImageContentContainer is the name of the container that will
	// pull down input images and extract their content for input to the build.
	ExtractImageContentContainer = "extract-image-content"

	// GitCloneContainer is the name of the container that will clone the
	// build source repository and also handle binary input content.
	GitCloneContainer = "git-clone"
)

const (
	CustomBuild = "custom-build"
	DockerBuild = "docker-build"
	StiBuild    = "sti-build"
)

var BuildContainerNames = []string{CustomBuild, StiBuild, DockerBuild}

var (
	// BuildControllerRefKind contains the schema.GroupVersionKind for builds.
	// This is used in the ownerRef of builder pods.
	BuildControllerRefKind = buildv1.GroupVersion.WithKind("Build")
)

// FatalError is an error which can't be retried.
type FatalError struct {
	// Reason the fatal error occurred
	Reason string
}

// Error implements the error interface.
func (e *FatalError) Error() string {
	return fmt.Sprintf("fatal error: %s", e.Reason)
}

// IsFatal returns true if the error is fatal
func IsFatal(err error) bool {
	_, isFatal := err.(*FatalError)
	return isFatal
}

// setupDockerSocket configures the pod to support the host's Docker socket
func setupDockerSocket(pod *corev1.Pod) {
	dockerSocketVolume := corev1.Volume{
		Name: "docker-socket",
		VolumeSource: corev1.VolumeSource{
			HostPath: &corev1.HostPathVolumeSource{
				Path: dockerSocketPath,
			},
		},
	}

	dockerSocketVolumeMount := corev1.VolumeMount{
		Name:      "docker-socket",
		MountPath: dockerSocketPath,
	}

	pod.Spec.Volumes = append(pod.Spec.Volumes,
		dockerSocketVolume)
	pod.Spec.Containers[0].VolumeMounts =
		append(pod.Spec.Containers[0].VolumeMounts,
			dockerSocketVolumeMount)
	for i, initContainer := range pod.Spec.InitContainers {
		if initContainer.Name == ExtractImageContentContainer {
			pod.Spec.InitContainers[i].VolumeMounts = append(pod.Spec.InitContainers[i].VolumeMounts, dockerSocketVolumeMount)
			break
		}
	}
}

// mountConfigMapVolume is a helper method responsible for actual mounting configMap
// volumes into a pod.
func mountConfigMapVolume(pod *corev1.Pod, container *corev1.Container, configMapName, mountPath, volumeSuffix string) {
	mountVolume(pod, container, configMapName, mountPath, volumeSuffix, policy.ConfigMap)
}

// mountSecretVolume is a helper method responsible for actual mounting secret
// volumes into a pod.
func mountSecretVolume(pod *corev1.Pod, container *corev1.Container, secretName, mountPath, volumeSuffix string) {
	mountVolume(pod, container, secretName, mountPath, volumeSuffix, policy.Secret)
}

// mountVolume is a helper method responsible for mounting volumes into a pod.
// The following file system types for the volume are supported:
//
// 1. ConfigMap
// 2. EmptyDir
// 3. Secret
func mountVolume(pod *corev1.Pod, container *corev1.Container, objName, mountPath, volumeSuffix string, fsType policy.FSType) {
	volumeName := apihelpers.GetName(objName, volumeSuffix, kvalidation.DNS1123LabelMaxLength)

	// coerce from RFC1123 subdomain to RFC1123 label.
	volumeName = strings.Replace(volumeName, ".", "-", -1)

	volumeExists := false
	for _, v := range pod.Spec.Volumes {
		if v.Name == volumeName {
			volumeExists = true
			break
		}
	}
	mode := int32(0600)
	if !volumeExists {
		volume := makeVolume(volumeName, objName, mode, fsType)
		pod.Spec.Volumes = append(pod.Spec.Volumes, volume)
	}

	volumeMount := corev1.VolumeMount{
		Name:      volumeName,
		MountPath: mountPath,
		ReadOnly:  true,
	}
	container.VolumeMounts = append(container.VolumeMounts, volumeMount)
}

func makeVolume(volumeName, refName string, mode int32, fsType policy.FSType) corev1.Volume {
	// TODO: Add support for key-based paths for secrets and configMaps?
	vol := corev1.Volume{
		Name:         volumeName,
		VolumeSource: corev1.VolumeSource{},
	}
	switch fsType {
	case policy.ConfigMap:
		vol.VolumeSource.ConfigMap = &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{
				Name: refName,
			},
			DefaultMode: &mode,
		}
	case policy.EmptyDir:
		vol.VolumeSource.EmptyDir = &corev1.EmptyDirVolumeSource{}
	case policy.Secret:
		vol.VolumeSource.Secret = &corev1.SecretVolumeSource{
			SecretName:  refName,
			DefaultMode: &mode,
		}
	default:
		glog.V(3).Infof("File system %s is not supported for volumes. Using empty directory instead.", fsType)
		vol.VolumeSource.EmptyDir = &corev1.EmptyDirVolumeSource{}
	}

	return vol
}

// setupDockerSecrets mounts Docker Registry secrets into Pod running the build,
// allowing Docker to authenticate against private registries or Docker Hub.
func setupDockerSecrets(pod *corev1.Pod, container *corev1.Container, pushSecret, pullSecret *corev1.LocalObjectReference, imageSources []buildv1.ImageSource) {
	if pushSecret != nil {
		mountSecretVolume(pod, container, pushSecret.Name, DockerPushSecretMountPath, "push")
		container.Env = append(container.Env, []corev1.EnvVar{
			{Name: "PUSH_DOCKERCFG_PATH", Value: DockerPushSecretMountPath},
		}...)
		glog.V(3).Infof("%s will be used for docker push in %s", DockerPushSecretMountPath, pod.Name)
	}

	if pullSecret != nil {
		mountSecretVolume(pod, container, pullSecret.Name, DockerPullSecretMountPath, "pull")
		container.Env = append(container.Env, []corev1.EnvVar{
			{Name: "PULL_DOCKERCFG_PATH", Value: DockerPullSecretMountPath},
		}...)
		glog.V(3).Infof("%s will be used for docker pull in %s", DockerPullSecretMountPath, pod.Name)
	}

	for i, imageSource := range imageSources {
		if imageSource.PullSecret == nil {
			continue
		}
		mountPath := filepath.Join(SourceImagePullSecretMountPath, strconv.Itoa(i))
		mountSecretVolume(pod, container, imageSource.PullSecret.Name, mountPath, fmt.Sprintf("%s%d", "source-image", i))
		container.Env = append(container.Env, []corev1.EnvVar{
			{Name: fmt.Sprintf("%s%d", "PULL_SOURCE_DOCKERCFG_PATH_", i), Value: mountPath},
		}...)
		glog.V(3).Infof("%s will be used for docker pull in %s", mountPath, pod.Name)
	}
}

// setupSourceSecrets mounts SSH key used for accessing private SCM to clone
// application source code during build.
func setupSourceSecrets(pod *corev1.Pod, container *corev1.Container, sourceSecret *corev1.LocalObjectReference) {
	if sourceSecret == nil {
		return
	}

	mountSecretVolume(pod, container, sourceSecret.Name, sourceSecretMountPath, "source")
	glog.V(3).Infof("Installed source secrets in %s, in Pod %s/%s", sourceSecretMountPath, pod.Namespace, pod.Name)
	container.Env = append(container.Env, []corev1.EnvVar{
		{Name: "SOURCE_SECRET_PATH", Value: sourceSecretMountPath},
	}...)
}

// setupInputConfigMaps mounts the configMaps referenced by the ConfigMapBuildSource
// into a builder container.
func setupInputConfigMaps(pod *corev1.Pod, container *corev1.Container, configs []buildv1.ConfigMapBuildSource) {
	for _, c := range configs {
		mountConfigMapVolume(pod, container, c.ConfigMap.Name, filepath.Join(ConfigMapBuildSourceBaseMountPath, c.ConfigMap.Name), "build")
		glog.V(3).Infof("%s will be used as a build config in %s", c.ConfigMap.Name, ConfigMapBuildSourceBaseMountPath)
	}
}

// setupInputSecrets mounts the secrets referenced by the SecretBuildSource
// into a builder container.
func setupInputSecrets(pod *corev1.Pod, container *corev1.Container, secrets []buildv1.SecretBuildSource) {
	for _, s := range secrets {
		mountSecretVolume(pod, container, s.Secret.Name, filepath.Join(SecretBuildSourceBaseMountPath, s.Secret.Name), "build")
		glog.V(3).Infof("%s will be used as a build secret in %s", s.Secret.Name, SecretBuildSourceBaseMountPath)
	}
}

// addSourceEnvVars adds environment variables related to the source code
// repository to builder container
func addSourceEnvVars(source buildv1.BuildSource, output *[]corev1.EnvVar) {
	sourceVars := []corev1.EnvVar{}
	if source.Git != nil {
		sourceVars = append(sourceVars, corev1.EnvVar{Name: "SOURCE_REPOSITORY", Value: source.Git.URI})
		sourceVars = append(sourceVars, corev1.EnvVar{Name: "SOURCE_URI", Value: source.Git.URI})
	}
	if len(source.ContextDir) > 0 {
		sourceVars = append(sourceVars, corev1.EnvVar{Name: "SOURCE_CONTEXT_DIR", Value: source.ContextDir})
	}
	if source.Git != nil && len(source.Git.Ref) > 0 {
		sourceVars = append(sourceVars, corev1.EnvVar{Name: "SOURCE_REF", Value: source.Git.Ref})
	}
	*output = append(*output, sourceVars...)
}

// addOutputEnvVars adds env variables that provide information about the output
// target for the build
func addOutputEnvVars(buildOutput *corev1.ObjectReference, output *[]corev1.EnvVar) error {
	if buildOutput == nil {
		return nil
	}

	// output must always be a DockerImage type reference at this point.
	if buildOutput.Kind != "DockerImage" {
		return fmt.Errorf("invalid build output kind %s, must be DockerImage", buildOutput.Kind)
	}
	ref, err := reference.Parse(buildOutput.Name)
	if err != nil {
		return err
	}
	registry := ref.Registry
	ref.Registry = ""
	image := ref.String()

	outputVars := []corev1.EnvVar{
		{Name: "OUTPUT_REGISTRY", Value: registry},
		{Name: "OUTPUT_IMAGE", Value: image},
	}

	*output = append(*output, outputVars...)
	return nil
}

// setupAdditionalSecrets creates secret volume mounts in the given pod for the given list of secrets
func setupAdditionalSecrets(pod *corev1.Pod, container *corev1.Container, secrets []buildv1.SecretSpec) {
	for _, secretSpec := range secrets {
		mountSecretVolume(pod, container, secretSpec.SecretSource.Name, secretSpec.MountPath, "secret")
		glog.V(3).Infof("Installed additional secret in %s, in Pod %s/%s", secretSpec.MountPath, pod.Namespace, pod.Name)
	}
}

// getPodLabels creates labels for the Build Pod
func getPodLabels(build *buildv1.Build) map[string]string {
	return map[string]string{buildutil.BuildLabel: buildapihelpers.LabelValue(build.Name)}
}

func makeOwnerReference(build *buildv1.Build) metav1.OwnerReference {
	t := true
	return metav1.OwnerReference{
		APIVersion: BuildControllerRefKind.GroupVersion().String(),
		Kind:       BuildControllerRefKind.Kind,
		Name:       build.Name,
		UID:        build.UID,
		Controller: &t,
	}
}

func setOwnerReference(pod *corev1.Pod, build *buildv1.Build) {
	pod.OwnerReferences = []metav1.OwnerReference{makeOwnerReference(build)}
}

// HasOwnerReference returns true if the build pod has an OwnerReference to the
// build.
func HasOwnerReference(pod *corev1.Pod, build *buildv1.Build) bool {
	ref := makeOwnerReference(build)

	for _, r := range pod.OwnerReferences {
		if reflect.DeepEqual(r, ref) {
			return true
		}
	}

	return false
}

// copyEnvVarSlice returns a copy of an []corev1.EnvVar
func copyEnvVarSlice(in []corev1.EnvVar) []corev1.EnvVar {
	out := make([]corev1.EnvVar, len(in))
	copy(out, in)
	return out
}

// setupContainersConfigs sets up volumes for mounting the node's configuration which governs which
// registries it knows about, whether or not they should be accessed with TLS, and signature policies.
func setupContainersConfigs(pod *corev1.Pod, container *corev1.Container) {
	configDir := ConfigMapBuildSystemConfigsMountPath
	optional := true
	volumeName := apihelpers.GetName("build-system-configs", "build", kvalidation.DNS1123LabelMaxLength)
	exists := false
	for _, v := range pod.Spec.Volumes {
		if v.Name == volumeName {
			exists = true
			break
		}
	}
	if !exists {
		pod.Spec.Volumes = append(pod.Spec.Volumes,
			corev1.Volume{
				Name: volumeName,
				VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: "build-system-configs",
						},
						Optional: &optional,
					},
				},
			},
		)
	}

	container.VolumeMounts = append(container.VolumeMounts,
		corev1.VolumeMount{
			Name:      volumeName,
			MountPath: configDir,
			ReadOnly:  true,
		},
	)

	registriesConfPath := filepath.Join(configDir, "registries.conf")
	registriesDirPath := filepath.Join(configDir, "registries.d")
	signaturePolicyPath := filepath.Join(configDir, "signature-policy.json")
	storageConfPath := filepath.Join(configDir, "storage.conf")

	container.Env = append(container.Env, corev1.EnvVar{Name: "BUILD_REGISTRIES_CONF_PATH", Value: registriesConfPath})
	container.Env = append(container.Env, corev1.EnvVar{Name: "BUILD_REGISTRIES_DIR_PATH", Value: registriesDirPath})
	container.Env = append(container.Env, corev1.EnvVar{Name: "BUILD_SIGNATURE_POLICY_PATH", Value: signaturePolicyPath})
	container.Env = append(container.Env, corev1.EnvVar{Name: "BUILD_STORAGE_CONF_PATH", Value: storageConfPath})
}

// setupContainersStorage creates a volume that we'll use for holding images and working
// root filesystems for building images.
func setupContainersStorage(pod *corev1.Pod, container *corev1.Container) {
	exists := false
	for _, v := range pod.Spec.Volumes {
		if v.Name == "container-storage-root" {
			exists = true
			break
		}
	}
	if !exists {
		pod.Spec.Volumes = append(pod.Spec.Volumes,
			corev1.Volume{
				Name: "container-storage-root",
				VolumeSource: corev1.VolumeSource{
					EmptyDir: &corev1.EmptyDirVolumeSource{},
				},
			},
		)
	}
	container.VolumeMounts = append(container.VolumeMounts,
		corev1.VolumeMount{
			Name:      "container-storage-root",
			MountPath: "/var/lib/containers/storage",
		},
	)
	container.Env = append(container.Env, corev1.EnvVar{Name: "BUILD_STORAGE_DRIVER", Value: "overlay"})
	container.Env = append(container.Env, corev1.EnvVar{Name: "BUILD_ISOLATION", Value: "chroot"})
}

// setupContainersNodeStorage borrows the appropriate storage directories from the node so
// that we can share layers that we're using with the node
func setupContainersNodeStorage(pod *corev1.Pod, container *corev1.Container) {
	exists := false
	for _, v := range pod.Spec.Volumes {
		if v.Name == "node-storage-root" {
			exists = true
			break
		}
	}
	if !exists {
		pod.Spec.Volumes = append(pod.Spec.Volumes,
			// TODO: run unprivileged https://github.com/openshift/origin/issues/662
			corev1.Volume{
				Name: "node-storage-root",
				VolumeSource: corev1.VolumeSource{
					HostPath: &corev1.HostPathVolumeSource{
						Path: "/var/lib/containers/storage",
					},
				},
			},
		)
	}
	container.VolumeMounts = append(container.VolumeMounts,
		// TODO: run unprivileged https://github.com/openshift/origin/issues/662
		corev1.VolumeMount{
			Name:      "node-storage-root",
			MountPath: "/var/lib/containers/storage",
		},
	)
	container.Env = append(container.Env, corev1.EnvVar{Name: "BUILD_STORAGE_DRIVER", Value: "overlay"})
	container.Env = append(container.Env, corev1.EnvVar{Name: "BUILD_ISOLATION", Value: "chroot"})
}

// setupBuildCAs mounts certificate authorities for the build from a predetermined ConfigMap.
func setupBuildCAs(build *buildv1.Build, pod *corev1.Pod) {
	casExist := false
	for _, v := range pod.Spec.Volumes {
		if v.Name == "build-ca-bundles" {
			casExist = true
			break
		}
	}

	if !casExist {
		pod.Spec.Volumes = append(pod.Spec.Volumes,
			corev1.Volume{
				Name: "build-ca-bundles",
				VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{
							Name: buildapihelpers.GetBuildCAConfigMapName(build),
						},
					},
				},
			},
		)
		containers := make([]corev1.Container, len(pod.Spec.Containers))
		for i, c := range pod.Spec.Containers {
			c.VolumeMounts = append(c.VolumeMounts,
				corev1.VolumeMount{
					Name:      "build-ca-bundles",
					MountPath: ConfigMapCertsMountPath,
				},
			)
			containers[i] = c
		}
		pod.Spec.Containers = containers
	}
}
