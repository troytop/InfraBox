package stub

import (
	goerr "errors"
	"github.com/sap/infrabox/src/controller/pkg/apis/core/v1alpha1"
	"strconv"

	"github.com/operator-framework/operator-sdk/pkg/sdk"
	"github.com/sirupsen/logrus"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/operator-framework/operator-sdk/pkg/k8sclient"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func (c *Controller) deletePipelineInvocation(cr *v1alpha1.IBPipelineInvocation, log *logrus.Entry) error {
	err := c.deleteServices(cr, log)
	if err != nil {
		log.Errorf("Failed to delete services: %v", err)
		return err
	}

	cr.SetFinalizers([]string{})
	err = updateStatus(cr, log)
	if err != nil {
		logrus.Errorf("Failed to remove finalizers: %v", err)
		return err
	}

	return nil
}

func (c *Controller) deleteService(pi *v1alpha1.IBPipelineInvocation, service *v1alpha1.IBPipelineService, log *logrus.Entry, index int) error {
	log.Infof("Deleting Service")
	id := pi.Name + "-" + strconv.Itoa(index)
	resourceClient, _, err := k8sclient.GetResourceClient(service.APIVersion, service.Kind, pi.Namespace)
	if err != nil {
		log.Errorf("failed to get resource client: %v", err)
		return err
	}

	err = resourceClient.Delete(id, metav1.NewDeleteOptions(0))
	if err != nil && !errors.IsNotFound(err) {
		log.Errorf("Failed to delete service: %s", err.Error())
		return err
	}

	return nil
}

func (c *Controller) deleteServices(pi *v1alpha1.IBPipelineInvocation, log *logrus.Entry) error {
	if pi.Spec.Services == nil {
		return nil
	}

	log.Info("Delete additional services")
	for index, s := range pi.Spec.Services {
		l := log.WithFields(logrus.Fields{
			"service_version": s.APIVersion,
			"service_kind":    s.Kind,
		})
		err := c.deleteService(pi, &s, l, index)
		if err != nil {
			return err
		}

		l.Info("Service deleted")
	}

	return nil
}

func (c *Controller) areServicesDeleted(pi *v1alpha1.IBPipelineInvocation, log *logrus.Entry) (bool, error) {
	if pi.Spec.Services == nil {
		return true, nil
	}

	log.Info("Delete additional services")
	for index, s := range pi.Spec.Services {
		id := pi.Name + "-" + strconv.Itoa(index)
		resourceClient, _, err := k8sclient.GetResourceClient(s.APIVersion, s.Kind, pi.Namespace)
		if err != nil {
			log.Errorf("failed to get resource client: %v", err)
			return false, err
		}

		service, err := resourceClient.Get(id, metav1.GetOptions{})
		log.Errorf("%v", err)
		log.Errorf("%v", service)

		if err == nil {
			// service still available
			return false, err
		}

		if err != nil {
			if errors.IsNotFound(err) {
				// already deleted
				continue
			} else {
				return false, err
			}
		}
	}

	return true, nil
}

func updateStatus(pi *v1alpha1.IBPipelineInvocation, log *logrus.Entry) error {
	resourceClient, _, err := k8sclient.GetResourceClient(pi.APIVersion, pi.Kind, pi.Namespace)
	if err != nil {
		log.Errorf("failed to get resource client: %v", err)
		return err
	}

	j, err := resourceClient.Get(pi.Name, metav1.GetOptions{})
	if err != nil {
		log.Errorf("failed to get pi: %v", err)
		return err
	}

	j.Object["status"] = pi.Status
	j.SetFinalizers(pi.GetFinalizers())
	_, err = resourceClient.Update(j)

	if err != nil {
		return err
	}

	return sdk.Get(pi)
}

func (c *Controller) preparePipelineInvocation(cr *v1alpha1.IBPipelineInvocation, log *logrus.Entry) error {
	logrus.Info("Prepare")
	cr.SetFinalizers([]string{"core.infrabox.net"})
	cr.Status.State = "preparing"
	cr.Status.Message = "Services are being created"
	err := updateStatus(cr, log)

	if err != nil {
		log.Warnf("Failed to update status: %v", err)
		return err
	}

	servicesCreated, err := c.createServices(cr, log)

	if err != nil {
		log.Errorf("Failed to create services: %s", err.Error())
		return err
	}

	if servicesCreated {
		log.Infof("Services are ready")
		cr.Status.Message = ""
		cr.Status.State = "scheduling"
	} else {
		log.Infof("Services not yet ready")
	}

	log.Info("Updating state")
	return updateStatus(cr, log)
}

func (c *Controller) runPipelineInvocation(cr *v1alpha1.IBPipelineInvocation, log *logrus.Entry) error {
	logrus.Info("Run")
	pipeline := newPipeline(cr)
	err := sdk.Get(pipeline)

	if err != nil {
		logrus.Errorf("Pipeline not found: ", cr.Spec.PipelineName)
		return err
	}

	// Sync all functions
	for index, pipelineStep := range pipeline.Spec.Steps {
		if len(cr.Status.StepStatuses) <= index {
			// No state yet for this step
			cr.Status.StepStatuses = append(cr.Status.StepStatuses, v1alpha1.IBFunctionInvocationStatus{
				State: corev1.ContainerState{
					Waiting: &corev1.ContainerStateWaiting{
						Message: "Containers are being created",
					},
				},
			})
		}

		status := &cr.Status.StepStatuses[index]

		if status.State.Terminated != nil {
			// step already finished
			log.Info("Step already finished")
			continue
		}

		stepInvocation, _ := cr.Spec.Steps[pipelineStep.Name]

		fi := newFunctionInvocation(cr, stepInvocation, &pipelineStep)
		err = sdk.Create(fi)

		if err != nil && !errors.IsAlreadyExists(err) {
			log.Errorf("Failed to create function invocation: %s", err.Error())
			return err
		}

		fi = newFunctionInvocation(cr, stepInvocation, &pipelineStep)
		err = sdk.Get(fi)
		if err != nil {
			return err
		}

		cr.Status.StepStatuses[index] = fi.Status
		if fi.Status.State.Terminated != nil {
			// don't continue with next step until this one finished
			break
		}
	}

	firstState := cr.Status.StepStatuses[0].State

	if firstState.Running != nil {
		cr.Status.Message = ""
		cr.Status.State = "running"
		cr.Status.StartTime = &firstState.Running.StartedAt
	} else if firstState.Terminated != nil {
		cr.Status.Message = ""
		cr.Status.State = "running"
		cr.Status.StartTime = &firstState.Terminated.StartedAt
	}

	// Determine current status
	allTerminated := true
	for _, stepStatus := range cr.Status.StepStatuses {
		if stepStatus.State.Terminated == nil {
			allTerminated = false
		}
	}

	if allTerminated {
		cr.Status.Message = ""
		cr.Status.State = "finalizing"
		cr.Status.StartTime = &firstState.Terminated.StartedAt
		cr.Status.CompletionTime = &cr.Status.StepStatuses[len(cr.Status.StepStatuses)-1].State.Terminated.FinishedAt
	}

	return updateStatus(cr, log)
}

func (c *Controller) finalizePipelineInvocation(cr *v1alpha1.IBPipelineInvocation, log *logrus.Entry) error {
	log.Info("Finalizing")

	err := c.deleteServices(cr, log)
	if err != nil {
		log.Errorf("Failed to delete services: %v", err)
		return err
	}

	allServicesDeleted, err := c.areServicesDeleted(cr, log)
	if err != nil {
		return err
	}

	if !allServicesDeleted {
		return nil
	}

	cr.Status.Message = ""
	cr.Status.State = "terminated"

	return updateStatus(cr, log)
}

func (c *Controller) createService(service *v1alpha1.IBPipelineService, pi *v1alpha1.IBPipelineInvocation, log *logrus.Entry, index int) (bool, error) {
	resourceClient, _, err := k8sclient.GetResourceClient(pi.APIVersion, pi.Kind, pi.Namespace)
	if err != nil {
		log.Errorf("failed to get resource client: %v", err)
		return false, err
	}

	j, err := resourceClient.Get(pi.Name, metav1.GetOptions{})
	if err != nil {
		log.Errorf("failed to get pi: %v", err)
		return false, err
	}

	services, ok := unstructured.NestedSlice(j.Object, "spec", "services")

	if !ok {
		return false, goerr.New("services not found")
	}

	var spec *map[string]interface{} = nil
	for _, ser := range services {
		m := ser.(map[string]interface{})
		un := unstructured.Unstructured{Object: m}
		name := un.GetName()

		if name == service.Metadata.Name {
			newSpec, ok := unstructured.NestedMap(m, "spec")

			if !ok {
				newSpec = make(map[string]interface{})
			}

			spec = &newSpec
		}
	}

	if spec == nil {
		return false, goerr.New("service not found")
	}

	id := pi.Name + "-" + strconv.Itoa(index)
	newService := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": service.APIVersion,
			"kind":       service.Kind,
			"metadata": map[string]interface{}{
				"name":        id,
				"namespace":   pi.Namespace,
				"annotations": service.Metadata.Annotations,
				"labels": map[string]string{
					"service.infrabox.net/secret-name": id,
				},
			},
			"spec": *spec,
		},
	}

	resourceClient, _, err = k8sclient.GetResourceClient(service.APIVersion, service.Kind, pi.Namespace)
	if err != nil {
		log.Errorf("failed to get resource client: %v", err)
		return false, err
	}

	_, err = resourceClient.Create(newService)
	if err != nil && !errors.IsAlreadyExists(err) {
		log.Errorf("Failed to post service: %s", err.Error())
		return false, err
	}

	log.Infof("Service %s/%s created", service.APIVersion, service.Kind)

	s, err := resourceClient.Get(id, metav1.GetOptions{})
	if err != nil {
		return false, err
	}

	status, ok := unstructured.NestedString(s.Object, "status", "status")

	if !ok {
		return false, nil
	}

	if status == "ready" {
		return true, nil
	}

	if status == "error" {
		msg, ok := unstructured.NestedString(s.Object, "status", "message")

		if !ok {
			msg = "Internal Error"
		}

		log.Errorf("service is in state error: %s", msg)
		return false, goerr.New(msg)
	}

	return false, nil
}

func (c *Controller) createServices(pi *v1alpha1.IBPipelineInvocation, log *logrus.Entry) (bool, error) {
	if pi.Spec.Services == nil {
		log.Info("No services specified")
		return true, nil
	}

	log.Info("Creating additional services")

	ready := true
	for index, s := range pi.Spec.Services {
		l := log.WithFields(logrus.Fields{
			"service_version": s.APIVersion,
			"service_kind":    s.Kind,
		})

		r, err := c.createService(&s, pi, l, index)

		if err != nil {
			l.Errorf("Failed to create service: %s", err.Error())
			return false, err
		}

		if r {
			l.Info("Service ready")
		} else {
			ready = false
			l.Infof("Service not yet ready")
		}
	}

	return ready, nil
}

func newPipeline(cr *v1alpha1.IBPipelineInvocation) *v1alpha1.IBPipeline {
	return &v1alpha1.IBPipeline{
		TypeMeta: metav1.TypeMeta{
			APIVersion: v1alpha1.SchemeGroupVersion.Group + "/" + v1alpha1.SchemeGroupVersion.Version,
			Kind:       "IBPipeline",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      cr.Spec.PipelineName,
			Namespace: cr.Namespace,
		},
	}
}

func newFunctionInvocation(pi *v1alpha1.IBPipelineInvocation,
	invocationStep v1alpha1.IBPipelineInvocationStep,
	step *v1alpha1.IBPipelineStep) *v1alpha1.IBFunctionInvocation {

	fi := &v1alpha1.IBFunctionInvocation{
		TypeMeta: metav1.TypeMeta{
			APIVersion: v1alpha1.SchemeGroupVersion.Group + "/" + v1alpha1.SchemeGroupVersion.Version,
			Kind:       "IBFunctionInvocation",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:            pi.Name + "-" + step.Name,
			Namespace:       pi.Namespace,
			OwnerReferences: newOwnerReferenceForPipelineInvocation(pi),
		},
		Spec: v1alpha1.IBFunctionInvocationSpec{
			FunctionName: step.FunctionName,
			Env:          invocationStep.Env,
		},
	}

	if invocationStep.Resources != nil {
		fi.Spec.Resources = invocationStep.Resources
	}

	for index, s := range pi.Spec.Services {
		id := pi.Name + "-" + strconv.Itoa(index)

		fi.Spec.Volumes = append(fi.Spec.Volumes, corev1.Volume{
			Name: id,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: id,
				},
			},
		})

		fi.Spec.VolumeMounts = append(fi.Spec.VolumeMounts, corev1.VolumeMount{
			Name:      id,
			MountPath: "/var/run/infrabox.net/services/" + s.Metadata.Name,
		})
	}

	return fi
}

func newOwnerReferenceForPipelineInvocation(cr *v1alpha1.IBPipelineInvocation) []metav1.OwnerReference {
	return []metav1.OwnerReference{
		*metav1.NewControllerRef(cr, schema.GroupVersionKind{
			Group:   v1alpha1.SchemeGroupVersion.Group,
			Version: v1alpha1.SchemeGroupVersion.Version,
			Kind:    "IBPipelineInvocation",
		}),
	}
}
