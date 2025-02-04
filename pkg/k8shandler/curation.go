package k8shandler

import (
	"fmt"
	"reflect"

	"github.com/openshift/cluster-logging-operator/pkg/utils"
	"github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/util/retry"

	logging "github.com/openshift/cluster-logging-operator/pkg/apis/logging/v1alpha1"
	sdk "github.com/operator-framework/operator-sdk/pkg/sdk"
	batchv1 "k8s.io/api/batch/v1"
	batch "k8s.io/api/batch/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Note: this will eventually be deprecated and functionality will be moved into the ES operator
//   in the case of Curator. Other curation deployments may not be supported in the future

const defaultSchedule = "30 3,9,15,21 * * *"

func CreateOrUpdateCuration(cluster *logging.ClusterLogging) (err error) {

	if cluster.Spec.Curation.Type == logging.CurationTypeCurator {

		if err = createOrUpdateCuratorServiceAccount(cluster); err != nil {
			return
		}

		if err = createOrUpdateCuratorConfigMap(cluster); err != nil {
			return
		}

		if err = createOrUpdateCuratorCronJob(cluster); err != nil {
			return
		}

		if err = createOrUpdateCuratorSecret(cluster); err != nil {
			return
		}

		curatorStatus, err := getCuratorStatus(cluster.Namespace)

		if err != nil {
			return fmt.Errorf("Failed to get status for Curator: %v", err)
		}

		printUpdateMessage := true
		retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			if exists, cluster := utils.DoesClusterLoggingExist(cluster); exists {
				if !reflect.DeepEqual(curatorStatus, cluster.Status.Curation.CuratorStatus) {
					if printUpdateMessage {
						logrus.Info("Updating status of Curator")
						printUpdateMessage = false
					}
					cluster.Status.Curation.CuratorStatus = curatorStatus
					return sdk.Update(cluster)
				}
			}
			return nil
		})
		if retryErr != nil {
			return fmt.Errorf("Failed to update Cluster Logging Curator status: %v", retryErr)
		}
	} else {
		removeCurator(cluster)
	}

	return nil
}

func removeCurator(cluster *logging.ClusterLogging) (err error) {
	if cluster.Spec.ManagementState == logging.ManagementStateManaged {
		if err = utils.RemoveServiceAccount(cluster, "curator"); err != nil {
			return
		}

		if err = utils.RemoveConfigMap(cluster, "curator"); err != nil {
			return
		}

		if err = utils.RemoveSecret(cluster, "curator"); err != nil {
			return
		}

		if err = utils.RemoveCronJob(cluster, "curator"); err != nil {
			return
		}
	}

	return nil
}

func createOrUpdateCuratorServiceAccount(logging *logging.ClusterLogging) error {

	curatorServiceAccount := utils.ServiceAccount("curator", logging.Namespace)

	utils.AddOwnerRefToObject(curatorServiceAccount, utils.AsOwner(logging))

	err := sdk.Create(curatorServiceAccount)
	if err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("Failure creating Curator service account: %v", err)
	}

	return nil
}

func createOrUpdateCuratorConfigMap(logging *logging.ClusterLogging) error {

	curatorConfigMap := utils.ConfigMap(
		"curator",
		logging.Namespace,
		map[string]string{
			"actions.yaml":  string(utils.GetFileContents("files/curator-actions.yaml")),
			"curator5.yaml": string(utils.GetFileContents("files/curator5-config.yaml")),
			"config.yaml":   string(utils.GetFileContents("files/curator-config.yaml")),
		},
	)

	utils.AddOwnerRefToObject(curatorConfigMap, utils.AsOwner(logging))

	err := sdk.Create(curatorConfigMap)
	if err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("Failure constructing Curator configmap: %v", err)
	}

	return nil
}

func createOrUpdateCuratorSecret(logging *logging.ClusterLogging) error {

	curatorSecret := utils.Secret(
		"curator",
		logging.Namespace,
		map[string][]byte{
			"ca":       utils.GetWorkingDirFileContents("ca.crt"),
			"key":      utils.GetWorkingDirFileContents("system.logging.curator.key"),
			"cert":     utils.GetWorkingDirFileContents("system.logging.curator.crt"),
			"ops-ca":   utils.GetWorkingDirFileContents("ca.crt"),
			"ops-key":  utils.GetWorkingDirFileContents("system.logging.curator.key"),
			"ops-cert": utils.GetWorkingDirFileContents("system.logging.curator.crt"),
		})

	utils.AddOwnerRefToObject(curatorSecret, utils.AsOwner(logging))

	err := utils.CreateOrUpdateSecret(curatorSecret)
	if err != nil {
		return err
	}

	return nil
}

func newCuratorCronJob(logging *logging.ClusterLogging, curatorName string, elasticsearchHost string) *batch.CronJob {
	var resources = logging.Spec.Curation.Resources
	if resources == nil {
		resources = &v1.ResourceRequirements{
			Limits: v1.ResourceList{v1.ResourceMemory: defaultCuratorMemory},
			Requests: v1.ResourceList{
				v1.ResourceMemory: defaultCuratorMemory,
				v1.ResourceCPU:    defaultCuratorCpuRequest,
			},
		}
	}
	curatorContainer := utils.Container("curator", v1.PullIfNotPresent, *resources)

	curatorContainer.Env = []v1.EnvVar{
		{Name: "K8S_HOST_URL", Value: "https://kubernetes.default.svc.cluster.local"},
		{Name: "ES_HOST", Value: elasticsearchHost},
		{Name: "ES_PORT", Value: "9200"},
		{Name: "ES_CLIENT_CERT", Value: "/etc/curator/keys/cert"},
		{Name: "ES_CLIENT_KEY", Value: "/etc/curator/keys/key"},
		{Name: "ES_CA", Value: "/etc/curator/keys/ca"},
		{Name: "CURATOR_DEFAULT_DAYS", Value: "30"},
		{Name: "CURATOR_SCRIPT_LOG_LEVEL", Value: "INFO"},
		{Name: "CURATOR_LOG_LEVEL", Value: "ERROR"},
		{Name: "CURATOR_TIMEOUT", Value: "300"},
	}

	curatorContainer.VolumeMounts = []v1.VolumeMount{
		{Name: "certs", ReadOnly: true, MountPath: "/etc/curator/keys"},
		{Name: "config", ReadOnly: true, MountPath: "/etc/curator/settings"},
	}

	curatorPodSpec := utils.PodSpec(
		"curator",
		[]v1.Container{curatorContainer},
		[]v1.Volume{
			{Name: "config", VolumeSource: v1.VolumeSource{ConfigMap: &v1.ConfigMapVolumeSource{LocalObjectReference: v1.LocalObjectReference{Name: "curator"}}}},
			{Name: "certs", VolumeSource: v1.VolumeSource{Secret: &v1.SecretVolumeSource{SecretName: "curator"}}},
		},
	)

	curatorPodSpec.RestartPolicy = v1.RestartPolicyNever
	curatorPodSpec.TerminationGracePeriodSeconds = utils.GetInt64(600)

	schedule := logging.Spec.Curation.CuratorSpec.Schedule
	if schedule == "" {
		schedule = defaultSchedule
	}

	curatorCronJob := utils.CronJob(
		curatorName,
		logging.Namespace,
		"curator",
		curatorName,
		batch.CronJobSpec{
			SuccessfulJobsHistoryLimit: utils.GetInt32(1),
			FailedJobsHistoryLimit:     utils.GetInt32(1),
			Schedule:                   schedule,
			JobTemplate: batch.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					BackoffLimit: utils.GetInt32(0),
					Parallelism:  utils.GetInt32(1),
					Template: v1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Name:      curatorName,
							Namespace: logging.Namespace,
							Labels: map[string]string{
								"provider":      "openshift",
								"component":     curatorName,
								"logging-infra": "curator",
							},
						},
						Spec: curatorPodSpec,
					},
				},
			},
		},
	)

	utils.AddOwnerRefToObject(curatorCronJob, utils.AsOwner(logging))

	return curatorCronJob
}

func createOrUpdateCuratorCronJob(cluster *logging.ClusterLogging) (err error) {

	if utils.AllInOne(cluster) {
		curatorCronJob := newCuratorCronJob(cluster, "curator", "elasticsearch")

		err = sdk.Create(curatorCronJob)
		if err != nil && !errors.IsAlreadyExists(err) {
			return fmt.Errorf("Failure constructing Curator cronjob: %v", err)
		}

		if cluster.Spec.ManagementState == logging.ManagementStateManaged {
			retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
				return updateCuratorIfRequired(curatorCronJob)
			})
			if retryErr != nil {
				return retryErr
			}
		}
	} else {
		curatorCronJob := newCuratorCronJob(cluster, "curator-app", "elasticsearch-app")

		err = sdk.Create(curatorCronJob)
		if err != nil && !errors.IsAlreadyExists(err) {
			return fmt.Errorf("Failure constructing Curator App cronjob: %v", err)
		}

		if cluster.Spec.ManagementState == logging.ManagementStateManaged {
			retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
				return updateCuratorIfRequired(curatorCronJob)
			})
			if retryErr != nil {
				return retryErr
			}
		}

		curatorInfraCronJob := newCuratorCronJob(cluster, "curator-infra", "elasticsearch-infra")

		err = sdk.Create(curatorInfraCronJob)
		if err != nil && !errors.IsAlreadyExists(err) {
			return fmt.Errorf("Failure constructing Curator Infra cronjob: %v", err)
		}

		if cluster.Spec.ManagementState == logging.ManagementStateManaged {
			retryErr := retry.RetryOnConflict(retry.DefaultRetry, func() error {
				return updateCuratorIfRequired(curatorInfraCronJob)
			})
			if retryErr != nil {
				return retryErr
			}
		}
	}

	return nil
}

func updateCuratorIfRequired(desired *batch.CronJob) (err error) {
	current := desired.DeepCopy()

	if err = sdk.Get(current); err != nil {
		if errors.IsNotFound(err) {
			// the object doesn't exist -- it was likely culled
			// recreate it on the next time through if necessary
			return nil
		}
		return fmt.Errorf("Failed to get Curator cronjob: %v", err)
	}

	current, different := isCuratorDifferent(current, desired)

	if different {
		if err = sdk.Update(current); err != nil {
			return err
		}
	}

	return nil
}

func isCuratorDifferent(current *batch.CronJob, desired *batch.CronJob) (*batch.CronJob, bool) {

	different := false

	// Check schedule
	if current.Spec.Schedule != desired.Spec.Schedule {
		logrus.Infof("Invalid Curator schedule found, updating %q", current.Name)
		current.Spec.Schedule = desired.Spec.Schedule
		different = true
	}

	// Check suspended
	if current.Spec.Suspend != nil && desired.Spec.Suspend != nil && *current.Spec.Suspend != *desired.Spec.Suspend {
		logrus.Infof("Invalid Curator suspend value found, updating %q", current.Name)
		current.Spec.Suspend = desired.Spec.Suspend
		different = true
	}

	if current.Spec.JobTemplate.Spec.Template.Spec.Containers[0].Image != desired.Spec.JobTemplate.Spec.Template.Spec.Containers[0].Image {
		logrus.Infof("Curator image change found, updating %q", current.Name)
		current.Spec.JobTemplate.Spec.Template.Spec.Containers[0].Image = desired.Spec.JobTemplate.Spec.Template.Spec.Containers[0].Image
		different = true
	}

	return current, different
}
