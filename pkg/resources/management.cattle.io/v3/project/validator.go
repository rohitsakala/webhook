package project

import (
	"fmt"

	v3 "github.com/rancher/rancher/pkg/apis/management.cattle.io/v3"
	"github.com/rancher/webhook/pkg/admission"
	objectsv3 "github.com/rancher/webhook/pkg/generated/objects/management.cattle.io/v3"
	"github.com/rancher/wrangler/pkg/data/convert"
	admissionv1 "k8s.io/api/admission/v1"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/kubernetes/pkg/kubelet/util/format"
	"k8s.io/utils/trace"
)

const (
	systemProjectLabel  = "authz.management.cattle.io/system-project"
	projectQuotaField   = "resourceQuota"
	namespaceQuotaField = "namespaceDefaultResourceQuota"
)

var projectSpecFieldPath = field.NewPath("project").Child("spec")

// Validator implements admission.ValidatingAdmissionWebhook.
type Validator struct {
	admitter admitter
}

// NewValidator returns a project validator.
func NewValidator() *Validator {
	return &Validator{}
}

// GVR returns the GroupVersionKind for this CRD.
func (v *Validator) GVR() schema.GroupVersionResource {
	return gvr
}

// Operations returns list of operations handled by this validator.
func (v *Validator) Operations() []admissionregistrationv1.OperationType {
	return []admissionregistrationv1.OperationType{
		admissionregistrationv1.Create,
		admissionregistrationv1.Update,
		admissionregistrationv1.Delete,
	}
}

// ValidatingWebhook returns the ValidatingWebhook used for this CRD.
func (v *Validator) ValidatingWebhook(clientConfig admissionregistrationv1.WebhookClientConfig) []admissionregistrationv1.ValidatingWebhook {
	validatingWebhook := admission.NewDefaultValidatingWebhook(v, clientConfig, admissionregistrationv1.NamespacedScope, v.Operations())
	return []admissionregistrationv1.ValidatingWebhook{*validatingWebhook}
}

// Admitters returns the admitter objects used to validate secrets.
func (v *Validator) Admitters() []admission.Admitter {
	return []admission.Admitter{&v.admitter}
}

type admitter struct{}

// Admit handles the webhook admission request sent to this webhook.
func (a *admitter) Admit(request *admission.Request) (*admissionv1.AdmissionResponse, error) {
	listTrace := trace.New("project Admit", trace.Field{Key: "user", Value: request.UserInfo.Username})
	defer listTrace.LogIfLong(admission.SlowTraceDuration)

	oldProject, newProject, err := objectsv3.ProjectOldAndNewFromRequest(&request.AdmissionRequest)
	if err != nil {
		return nil, fmt.Errorf("failed to get old and new projects from request: %w", err)
	}

	if request.Operation == admissionv1.Delete {
		return a.admitDelete(oldProject)
	}
	return a.admitCreateOrUpdate(oldProject, newProject)
}

func (a *admitter) admitDelete(project *v3.Project) (*admissionv1.AdmissionResponse, error) {
	if project.Labels[systemProjectLabel] == "true" {
		return admission.ResponseBadRequest("System Project cannot be deleted"), nil
	}
	return admission.ResponseAllowed(), nil
}

func (a *admitter) admitCreateOrUpdate(oldProject, newProject *v3.Project) (*admissionv1.AdmissionResponse, error) {
	projectQuota := newProject.Spec.ResourceQuota
	nsQuota := newProject.Spec.NamespaceDefaultResourceQuota
	if projectQuota == nil && nsQuota == nil {
		return admission.ResponseAllowed(), nil
	}
	fieldErr, err := checkQuotaFields(projectQuota, nsQuota)
	if err != nil {
		return nil, fmt.Errorf("error checking project fields: %w", err)
	}
	if fieldErr != nil {
		return admission.ResponseBadRequest(fieldErr.Error()), nil
	}
	fieldErr, err = a.checkQuotaValues(&nsQuota.Limit, &projectQuota.Limit, oldProject)
	if err != nil {
		return nil, fmt.Errorf("error checking quota values: %w", err)
	}
	if fieldErr != nil {
		return admission.ResponseBadRequest(fieldErr.Error()), nil
	}
	return admission.ResponseAllowed(), nil
}

func checkQuotaFields(projectQuota *v3.ProjectResourceQuota, nsQuota *v3.NamespaceResourceQuota) (*field.Error, error) {

	if projectQuota == nil && nsQuota != nil {
		return field.Required(projectSpecFieldPath.Child(projectQuotaField), fmt.Sprintf("required when %s is set", namespaceQuotaField)), nil
	}
	if projectQuota != nil && nsQuota == nil {
		return field.Required(projectSpecFieldPath.Child(namespaceQuotaField), fmt.Sprintf("required when %s is set", projectQuotaField)), nil
	}

	projectQuotaLimitMap, err := convert.EncodeToMap(projectQuota.Limit)
	if err != nil {
		return nil, fmt.Errorf("failed to decode project quota limit: %w", err)
	}
	nsQuotaLimitMap, err := convert.EncodeToMap(nsQuota.Limit)
	if err != nil {
		return nil, fmt.Errorf("failed to decode namespace default quota limit: %w", err)
	}
	if len(projectQuotaLimitMap) != len(nsQuotaLimitMap) {
		return field.Invalid(projectSpecFieldPath.Child(projectQuotaField), projectQuota, "resource quota and namespace default quota do not have the same resources defined"), nil
	}
	for k := range projectQuotaLimitMap {
		if _, ok := nsQuotaLimitMap[k]; !ok {
			return field.Invalid(projectSpecFieldPath.Child(namespaceQuotaField), nsQuota, fmt.Sprintf("missing namespace default for resource %s defined on %s", k, projectQuotaField)), nil
		}
	}
	return nil, nil
}

func (a *admitter) checkQuotaValues(nsQuota, projectQuota *v3.ResourceQuotaLimit, oldProject *v3.Project) (*field.Error, error) {
	// check quota on new project
	fieldErr, err := namespaceQuotaFits(nsQuota, projectQuota)
	if err != nil || fieldErr != nil {
		return fieldErr, err
	}

	// if there is no old project or no quota on the old project, no further validation needed
	if oldProject == nil || oldProject.Spec.ResourceQuota == nil {
		return nil, nil
	}

	// check quota relative to used quota
	return usedQuotaFits(&oldProject.Spec.ResourceQuota.UsedLimit, projectQuota)
}

func namespaceQuotaFits(namespaceQuota, projectQuota *v3.ResourceQuotaLimit) (*field.Error, error) {
	namespaceQuotaResourceList, err := convertLimitToResourceList(namespaceQuota)
	if err != nil {
		return nil, err
	}
	projectQuotaResourceList, err := convertLimitToResourceList(projectQuota)
	if err != nil {
		return nil, err
	}
	fits, exceeded := quotaFits(namespaceQuotaResourceList, projectQuotaResourceList)
	if !fits {
		return field.Forbidden(projectSpecFieldPath.Child(namespaceQuotaField), fmt.Sprintf("namespace default quota limit exceeds project limit on fields: %s", format.ResourceList(exceeded))), nil
	}
	return nil, nil
}

func usedQuotaFits(usedQuota, projectQuota *v3.ResourceQuotaLimit) (*field.Error, error) {
	usedQuotaResourceList, err := convertLimitToResourceList(usedQuota)
	if err != nil {
		return nil, err
	}
	projectQuotaResourceList, err := convertLimitToResourceList(projectQuota)
	if err != nil {
		return nil, err
	}
	fits, exceeded := quotaFits(usedQuotaResourceList, projectQuotaResourceList)
	if !fits {
		return field.Forbidden(projectSpecFieldPath.Child(projectQuotaField), fmt.Sprintf("resourceQuota is below the used limit on fields: %s", format.ResourceList(exceeded))), nil
	}
	return nil, nil
}
