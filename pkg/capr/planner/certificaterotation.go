package planner

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"

	rkev1 "github.com/rancher/rancher/pkg/apis/rke.cattle.io/v1"
	"github.com/rancher/rancher/pkg/apis/rke.cattle.io/v1/plan"
	"github.com/rancher/rancher/pkg/capr"
	"github.com/sirupsen/logrus"
)

// rotateCertificates checks if there is a need to rotate any certificates and updates the plan accordingly.
func (p *Planner) rotateCertificates(controlPlane *rkev1.RKEControlPlane, status rkev1.RKEControlPlaneStatus, tokensSecret plan.Secret, clusterPlan *plan.Plan) (rkev1.RKEControlPlaneStatus, error) {
	if !shouldRotate(controlPlane) {
		return status, nil
	}

	found, joinServer, _, err := p.findInitNode(controlPlane, clusterPlan)
	if err != nil {
		logrus.Errorf("[planner] rkecluster %s/%s: error encountered while searching for init node during certificate rotation: %v", controlPlane.Namespace, controlPlane.Name, err)
		return status, err
	}
	if !found || joinServer == "" {
		logrus.Warnf("[planner] rkecluster %s/%s: skipping certificate creation as cluster does not have an init node", controlPlane.Namespace, controlPlane.Name)
		return status, nil
	}

	for _, node := range collect(clusterPlan, anyRole) {
		if !shouldRotateEntry(controlPlane.Spec.RotateCertificates, node) {
			continue
		}

		rotatePlan, joinedServer, err := p.rotateCertificatesPlan(controlPlane, tokensSecret, controlPlane.Spec.RotateCertificates, node, joinServer)
		if err != nil {
			return status, err
		}

		err = assignAndCheckPlan(p.store, fmt.Sprintf("[%s] certificate rotation", node.Machine.Name), node, rotatePlan, joinedServer, 0, 0)
		if err != nil {
			// Ensure the CAPI cluster is paused if we have assigned and are checking a plan.
			if pauseErr := p.pauseCAPICluster(controlPlane, true); pauseErr != nil {
				return status, pauseErr
			}
			return status, err
		}
	}

	if err := p.pauseCAPICluster(controlPlane, false); err != nil {
		return status, errWaiting("unpausing CAPI cluster")
	}

	status.CertificateRotationGeneration = controlPlane.Spec.RotateCertificates.Generation
	return status, errWaiting("certificate rotation done")
}

// shouldRotate `true` if the cluster is ready and the generation is stale
func shouldRotate(cp *rkev1.RKEControlPlane) bool {
	// if a spec is not defined there is nothing to do
	if cp.Spec.RotateCertificates == nil {
		return false
	}

	// The controlplane must be initialized before we rotate anything
	if cp.Status.Initialized != true {
		logrus.Warnf("[planner] rkecluster %s/%s: skipping certificate rotation as cluster was not initialized", cp.Namespace, cp.Name)
		return false
	}

	// if this generation has already been applied there is no work
	return cp.Status.CertificateRotationGeneration != cp.Spec.RotateCertificates.Generation
}

const idempotentRotateScript = `
#!/bin/sh

currentGeneration=""
targetGeneration=$2
runtime=$1
shift
shift

dataRoot="/var/lib/rancher/$runtime/certificate_rotation"
generationFile="$dataRoot/generation"

currentGeneration=$(cat "$generationFile" || echo "")

if [ "$currentGeneration" != "$targetGeneration" ]; then
  $runtime certificate rotate  $@
else
	echo "certificates have already been rotated to the current generation."
fi

mkdir -p $dataRoot
echo $targetGeneration > "$generationFile"
`

// rotateCertificatesPlan rotates the certificates for the services specified, if any, and restarts the service.  If no services are specified
// all certificates are rotated.
func (p *Planner) rotateCertificatesPlan(controlPlane *rkev1.RKEControlPlane, tokensSecret plan.Secret, rotation *rkev1.RotateCertificates, entry *planEntry, joinServer string) (plan.NodePlan, string, error) {
	if isOnlyWorker(entry) {
		// Don't overwrite the joinURL annotation.
		joinServer = ""
	}
	rotatePlan, config, joinedServer, err := p.generatePlanWithConfigFiles(controlPlane, tokensSecret, entry, joinServer, true)
	if err != nil {
		return plan.NodePlan{}, joinedServer, err
	}

	if isOnlyWorker(entry) {
		rotatePlan.Instructions = append(rotatePlan.Instructions, plan.OneTimeInstruction{
			Name:    "restart",
			Command: "systemctl",
			Args: []string{
				"restart",
				capr.GetRuntimeAgentUnit(controlPlane.Spec.KubernetesVersion),
			},
		})
		return rotatePlan, joinedServer, nil
	}

	rotateScriptPath := "/var/lib/rancher/" + capr.GetRuntime(controlPlane.Spec.KubernetesVersion) + "/rancher_v2prov_certificate_rotation/bin/rotate.sh"

	runtime := capr.GetRuntime(controlPlane.Spec.KubernetesVersion)

	args := []string{
		"-xe",
		rotateScriptPath,
		capr.GetRuntime(controlPlane.Spec.KubernetesVersion),
		strconv.FormatInt(rotation.Generation, 10),
	}

	if len(rotation.Services) > 0 {
		for _, service := range rotation.Services {
			args = append(args, "-s", service)
		}
	}

	rotatePlan.Files = append(rotatePlan.Files, plan.File{
		Content: base64.StdEncoding.EncodeToString([]byte(idempotentRotateScript)),
		Path:    rotateScriptPath,
	})
	rotatePlan.Instructions = append(rotatePlan.Instructions, plan.OneTimeInstruction{
		Name:    "rotate certificates",
		Command: "sh",
		Args:    args,
	})
	if isControlPlane(entry) {
		// The following kube-scheduler and kube-controller-manager certificates are self-signed by the respective services and are used by CAPR for secure healthz probes against the service.
		if rotationContainsService(rotation, "controller-manager") {
			if kcmCertDir := getArgValue(config[KubeControllerManagerArg], CertDirArgument, "="); kcmCertDir != "" && getArgValue(config[KubeControllerManagerArg], TLSCertFileArgument, "=") == "" {
				rotatePlan.Instructions = append(rotatePlan.Instructions, []plan.OneTimeInstruction{
					{
						Name:    "remove kube-controller-manager cert for regeneration",
						Command: "rm",
						Args: []string{
							"-f",
							fmt.Sprintf("%s/%s", kcmCertDir, DefaultKubeControllerManagerCert),
						},
					},
					{
						Name:    "remove kube-controller-manager key for regeneration",
						Command: "rm",
						Args: []string{
							"-f",
							fmt.Sprintf("%s/%s", kcmCertDir, strings.ReplaceAll(DefaultKubeControllerManagerCert, ".crt", ".key")),
						},
					},
				}...)
				if runtime == capr.RuntimeRKE2 {
					rotatePlan.Instructions = append(rotatePlan.Instructions, plan.OneTimeInstruction{
						Name:    "remove kube-controller-manager static pod manifest",
						Command: "rm",
						Args: []string{
							"-f",
							"/var/lib/rancher/rke2/agent/pod-manifests/kube-controller-manager.yaml",
						},
					})
				}
			}
		}
		if rotationContainsService(rotation, "scheduler") {
			if ksCertDir := getArgValue(config[KubeSchedulerArg], CertDirArgument, "="); ksCertDir != "" && getArgValue(config[KubeSchedulerArg], TLSCertFileArgument, "=") == "" {
				rotatePlan.Instructions = append(rotatePlan.Instructions, []plan.OneTimeInstruction{
					{
						Name:    "remove kube-scheduler cert for regeneration",
						Command: "rm",
						Args: []string{
							"-f",
							fmt.Sprintf("%s/%s", ksCertDir, KubeSchedulerArg),
						},
					},
					{
						Name:    "remove kube-scheduler key for regeneration",
						Command: "rm",
						Args: []string{
							"-f",
							fmt.Sprintf("%s/%s", ksCertDir, strings.ReplaceAll(KubeSchedulerArg, ".crt", ".key")),
						},
					},
				}...)
				if runtime == capr.RuntimeRKE2 {
					rotatePlan.Instructions = append(rotatePlan.Instructions, plan.OneTimeInstruction{
						Name:    "remove kube-scheduler static pod manifest",
						Command: "rm",
						Args: []string{
							"-f",
							"/var/lib/rancher/rke2/agent/pod-manifests/kube-scheduler.yaml",
						},
					})
				}
			}
		}
	}
	if runtime == capr.RuntimeRKE2 {
		if generated, instruction := generateManifestRemovalInstruction(runtime, entry); generated {
			rotatePlan.Instructions = append(rotatePlan.Instructions, instruction)
		}
	}
	rotatePlan.Instructions = append(rotatePlan.Instructions, plan.OneTimeInstruction{
		Name:    "restart",
		Command: "systemctl",
		Args: []string{
			"restart",
			capr.GetRuntimeServerUnit(controlPlane.Spec.KubernetesVersion),
		},
	})
	return rotatePlan, joinedServer, nil
}

// rotationContainsService searches the rotation.Services slice the specified service. If the length of the services slice is 0, it returns true.
func rotationContainsService(rotation *rkev1.RotateCertificates, service string) bool {
	if rotation == nil {
		return false
	}
	if len(rotation.Services) == 0 {
		return true
	}
	for _, desiredService := range rotation.Services {
		if desiredService == service {
			return true
		}
	}
	return false
}

// shouldRotateEntry returns true if the rotated services are applicable to the entry's roles.
func shouldRotateEntry(rotation *rkev1.RotateCertificates, entry *planEntry) bool {
	relevantServices := map[string]struct{}{}

	if len(rotation.Services) == 0 {
		return true
	}

	if isWorker(entry) {
		relevantServices["rke2-server"] = struct{}{}
		relevantServices["k3s-server"] = struct{}{}
		relevantServices["api-server"] = struct{}{}
		relevantServices["kubelet"] = struct{}{}
		relevantServices["kube-proxy"] = struct{}{}
		relevantServices["auth-proxy"] = struct{}{}
	}

	if isControlPlane(entry) {
		relevantServices["rke2-server"] = struct{}{}
		relevantServices["k3s-server"] = struct{}{}
		relevantServices["api-server"] = struct{}{}
		relevantServices["kubelet"] = struct{}{}
		relevantServices["kube-proxy"] = struct{}{}
		relevantServices["auth-proxy"] = struct{}{}
		relevantServices["controller-manager"] = struct{}{}
		relevantServices["scheduler"] = struct{}{}
		relevantServices["rke2-controller"] = struct{}{}
		relevantServices["k3s-controller"] = struct{}{}
		relevantServices["admin"] = struct{}{}
		relevantServices["cloud-controller"] = struct{}{}
	}

	if isEtcd(entry) {
		relevantServices["etcd"] = struct{}{}
		relevantServices["kubelet"] = struct{}{}
		relevantServices["k3s-server"] = struct{}{}
		relevantServices["rke2-server"] = struct{}{}
	}

	for i := range rotation.Services {
		if _, ok := relevantServices[rotation.Services[i]]; ok {
			return true
		}
	}

	return false
}
