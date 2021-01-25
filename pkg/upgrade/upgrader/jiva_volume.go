/*
Copyright 2020 The OpenEBS Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package upgrader

import (
	v1Alpha1API "github.com/openebs/api/v2/pkg/apis/openebs.io/v1alpha1"
	"github.com/openebs/upgrade/pkg/upgrade/patch"
	"github.com/pkg/errors"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

// JivaVolumePatch is the patch required to upgrade JivaVolume
type JivaVolumePatch struct {
	*ResourcePatch
	Namespace  string
	Controller *patch.Deployment
	Replicas   *patch.StatefulSet
	Service    *patch.Service
	Utask      *v1Alpha1API.UpgradeTask
	*Client
}

// JivaVolumePatchOptions ...
type JivaVolumePatchOptions func(*JivaVolumePatch)

// WithJivaVolumeResorcePatch ...
func WithJivaVolumeResorcePatch(r *ResourcePatch) JivaVolumePatchOptions {
	return func(obj *JivaVolumePatch) {
		obj.ResourcePatch = r
	}
}

// WithJivaVolumeClient ...
func WithJivaVolumeClient(c *Client) JivaVolumePatchOptions {
	return func(obj *JivaVolumePatch) {
		obj.Client = c
	}
}

// NewJivaVolumePatch ...
func NewJivaVolumePatch(opts ...JivaVolumePatchOptions) *JivaVolumePatch {
	obj := &JivaVolumePatch{}
	for _, o := range opts {
		o(obj)
	}
	return obj
}

// PreUpgrade ...
func (obj *JivaVolumePatch) PreUpgrade() (string, error) {
	err := isOperatorUpgraded("cvc-operator", obj.Namespace, obj.To, obj.KubeClientset)
	if err != nil {
		return "failed to verify cvc-operator", err
	}
	err = obj.Controller.PreChecks(obj.From, obj.To)
	if err != nil {
		return "failed to verify controller deploy", err
	}
	err = obj.Replicas.PreChecks(obj.From, obj.To)
	if err != nil {
		return "failed to verify replica statefulset", err
	}
	err = obj.Service.PreChecks(obj.From, obj.To)
	if err != nil {
		return "failed to verify target svc", err
	}
	return "", nil
}

// Init initializes all the fields of the JivaVolumePatch
func (obj *JivaVolumePatch) Init() (string, error) {
	pvLabel := "openebs.io/persistent-volume=" + obj.Name
	replicaLabel := "openebs.io/replica=jiva-replica," + pvLabel
	controllerLabel := "openebs.io/controller=jiva-controller," + pvLabel
	serviceLabel := "openebs.io/controller-service=jiva-controller-svc," + pvLabel
	obj.Namespace = obj.OpenebsNamespace
	obj.Controller = patch.NewDeployment(
		patch.WithDeploymentClient(obj.KubeClientset),
	)
	err := obj.Controller.Get(controllerLabel, obj.Namespace)
	if err != nil {
		return "failed to get controller deployment for volume" + obj.Name, err
	}
	obj.Replicas = patch.NewStatefulSet(
		patch.WithStatefulSetClient(obj.KubeClientset),
	)
	err = obj.Replicas.Get(replicaLabel, obj.Namespace)
	if err != nil {
		return "failed to list replica statefulset for volume" + obj.Name, err
	}
	obj.Service = patch.NewService(
		patch.WithKubeClient(obj.KubeClientset),
	)
	err = obj.Service.Get(serviceLabel, obj.Namespace)
	if err != nil {
		return "failed to get target svc for volume" + obj.Name, err
	}
	err = obj.getJivaControllerPatchData()
	if err != nil {
		return "failed to create target deploy patch for volume" + obj.Name, err
	}
	err = getJivaServicePatchData(obj)
	if err != nil {
		return "failed to create target svc patch for volume" + obj.Name, err
	}
	return "", nil
}

func (obj *JivaVolumePatch) getJivaControllerPatchData() error {
	newDeploy := obj.Controller.Object.DeepCopy()
	err := obj.transformJivaController(newDeploy, obj.ResourcePatch)
	if err != nil {
		return err
	}
	obj.Controller.Data, err = GetPatchData(obj.Controller.Object, newDeploy)
	return err
}

func (obj *JivaVolumePatch) transformJivaController(d *appsv1.Deployment, res *ResourcePatch) error {
	// update deployment images
	tag := res.To
	if res.ImageTag != "" {
		tag = res.ImageTag
	}
	cons := len(d.Spec.Template.Spec.Containers)
	for i := 0; i < cons; i++ {
		url, err := getImageURL(
			d.Spec.Template.Spec.Containers[i].Image,
			res.BaseURL,
		)
		if err != nil {
			return err
		}
		d.Spec.Template.Spec.Containers[i].Image = url + ":" + tag
	}
	d.Labels["openebs.io/version"] = res.To
	d.Spec.Template.Labels["openebs.io/version"] = res.To
	return nil
}

func getJivaServicePatchData(obj *JivaVolumePatch) error {
	newSVC := obj.Service.Object.DeepCopy()
	err := transformJivaService(newSVC, obj.ResourcePatch)
	if err != nil {
		return err
	}
	obj.Service.Data, err = GetPatchData(obj.Service.Object, newSVC)
	return err
}

func transformJivaService(svc *corev1.Service, res *ResourcePatch) error {
	svc.Labels["openebs.io/version"] = res.To
	return nil
}

// JivaVolumeUpgrade ...
func (obj *JivaVolumePatch) JivaVolumeUpgrade() (string, error) {
	err := obj.Controller.Patch(obj.From, obj.To)
	if err != nil {
		return "failed to patch target deploy", err
	}
	err = obj.Service.Patch(obj.From, obj.To)
	if err != nil {
		return "failed to patch target svc", err
	}
	err = obj.Replicas.Patch(obj.From, obj.To)
	if err != nil {
		return "failed to patch Jiva", err
	}
	// err = obj.JivaVolumeCR.Patch(obj.From, obj.To)
	// if err != nil {
	// 	return "failed to patch JivaCR", err
	// }
	// err = obj.verifyJivaVolumeCRversionReconcile()
	// if err != nil {
	// 	return "failed to verify version reconcile on JivaVolumeCR", err
	// }
	return "", nil
}

// Upgrade execute the steps to upgrade JivaVolume
func (obj *JivaVolumePatch) Upgrade() error {
	var err, uerr error
	obj.Utask, err = getOrCreateUpgradeTask(
		"cstorVolume",
		obj.ResourcePatch,
		obj.Client,
	)
	if uerr != nil && isUpgradeTaskJob {
		return uerr
	}
	statusObj := v1Alpha1API.UpgradeDetailedStatuses{Step: v1Alpha1API.PreUpgrade}
	statusObj.Phase = v1Alpha1API.StepWaiting
	obj.Utask, uerr = updateUpgradeDetailedStatus(obj.Utask, statusObj, obj.OpenebsNamespace, obj.Client)
	if uerr != nil && isUpgradeTaskJob {
		return uerr
	}
	statusObj.Phase = v1Alpha1API.StepErrored
	msg, err := obj.Init()
	if err != nil {
		statusObj.Message = msg
		statusObj.Reason = err.Error()
		obj.Utask, uerr = updateUpgradeDetailedStatus(obj.Utask, statusObj, obj.OpenebsNamespace, obj.Client)
		if uerr != nil && isUpgradeTaskJob {
			return uerr
		}
		return errors.Wrap(err, msg)
	}
	msg, err = obj.PreUpgrade()
	if err != nil {
		statusObj.Message = msg
		statusObj.Reason = err.Error()
		obj.Utask, uerr = updateUpgradeDetailedStatus(obj.Utask, statusObj, obj.OpenebsNamespace, obj.Client)
		if uerr != nil && isUpgradeTaskJob {
			return uerr
		}
		return errors.Wrap(err, msg)
	}
	statusObj.Phase = v1Alpha1API.StepCompleted
	statusObj.Message = "Pre-upgrade steps were successful"
	statusObj.Reason = ""
	obj.Utask, uerr = updateUpgradeDetailedStatus(obj.Utask, statusObj, obj.OpenebsNamespace, obj.Client)
	if uerr != nil && isUpgradeTaskJob {
		return uerr
	}

	statusObj = v1Alpha1API.UpgradeDetailedStatuses{Step: v1Alpha1API.ReplicaUpgrade}
	statusObj.Phase = v1Alpha1API.StepWaiting
	obj.Utask, uerr = updateUpgradeDetailedStatus(obj.Utask, statusObj, obj.OpenebsNamespace, obj.Client)
	if uerr != nil && isUpgradeTaskJob {
		return uerr
	}
	statusObj.Phase = v1Alpha1API.StepErrored

	statusObj.Phase = v1Alpha1API.StepCompleted
	statusObj.Message = "Replica upgrade was successful"
	statusObj.Reason = ""
	obj.Utask, uerr = updateUpgradeDetailedStatus(obj.Utask, statusObj, obj.OpenebsNamespace, obj.Client)
	if uerr != nil && isUpgradeTaskJob {
		return uerr
	}
	statusObj = v1Alpha1API.UpgradeDetailedStatuses{Step: v1Alpha1API.TargetUpgrade}
	statusObj.Phase = v1Alpha1API.StepWaiting
	obj.Utask, uerr = updateUpgradeDetailedStatus(obj.Utask, statusObj, obj.OpenebsNamespace, obj.Client)
	if uerr != nil && isUpgradeTaskJob {
		return uerr
	}
	statusObj.Phase = v1Alpha1API.StepErrored
	msg, err = obj.JivaVolumeUpgrade()
	if err != nil {
		statusObj.Message = msg
		statusObj.Reason = err.Error()
		obj.Utask, uerr = updateUpgradeDetailedStatus(obj.Utask, statusObj, obj.OpenebsNamespace, obj.Client)
		if uerr != nil && isUpgradeTaskJob {
			return uerr
		}
		return errors.Wrap(err, msg)
	}
	statusObj.Phase = v1Alpha1API.StepCompleted
	statusObj.Message = "Target upgrade was successful"
	statusObj.Reason = ""
	obj.Utask, uerr = updateUpgradeDetailedStatus(obj.Utask, statusObj, obj.OpenebsNamespace, obj.Client)
	if uerr != nil && isUpgradeTaskJob {
		return uerr
	}
	return nil
}