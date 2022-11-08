/*
Copyright 2018 Google LLC

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

package securitypolicy

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/dgrijalva/jwt-go"
	"github.com/golang/glog"
	"github.com/pkg/errors"
	gcpjwt "github.com/someone1/gcp-jwt-go"
	"google.golang.org/genproto/googleapis/devtools/containeranalysis/v1beta1/vulnerability"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"

	"github.com/grafeas/kritis/pkg/kritis/apis/kritis/v1beta1"
	"github.com/grafeas/kritis/pkg/kritis/binauthz"
	clientset "github.com/grafeas/kritis/pkg/kritis/client/clientset/versioned"
	"github.com/grafeas/kritis/pkg/kritis/constants"
	"github.com/grafeas/kritis/pkg/kritis/container"
	"github.com/grafeas/kritis/pkg/kritis/kubectl/plugins/resolve"
	"github.com/grafeas/kritis/pkg/kritis/metadata"
	"github.com/grafeas/kritis/pkg/kritis/policy"
)

// ValidateFunc defines the type for Validating Image Security Policies
type ValidateFunc func(isp v1beta1.ImageSecurityPolicy, image string, metadataFetcher metadata.Fetcher, attestorFetcher AttestorFetcher) ([]policy.Violation, error)

// ImageSecurityPolicies returns all ISPs in the specified namespaces
// Pass in an empty string to get all ISPs in all namespaces
func ImageSecurityPolicies(namespace string) ([]v1beta1.ImageSecurityPolicy, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, errors.Wrap(err, "error building config")
	}

	client, err := clientset.NewForConfig(config)
	if err != nil {
		return nil, errors.Wrap(err, "error building clientset")
	}
	list, err := client.KritisV1beta1().ImageSecurityPolicies(namespace).List(metav1.ListOptions{})
	if err != nil {
		return nil, errors.Wrap(err, "error listing all image security policies")
	}
	return list.Items, nil
}

// ValidateImageSecurityPolicy checks if an image satisfies ISP requirements
// It returns a list of vulnerabilities that don't pass
func ValidateImageSecurityPolicy(isp v1beta1.ImageSecurityPolicy, image string, metadataFetcher metadata.Fetcher, attestorFetcher AttestorFetcher) ([]policy.Violation, error) {
	// First, check if image is whitelisted
	if imageInWhitelist(isp, image) {
		glog.Infof("%q is whitelisted in ImageSecurityPolicy", image)
		return nil, nil
	}
	var violations []policy.Violation
	// Next, check if image in qualified
	if !resolve.FullyQualifiedImage(image) {
		violations = append(violations, Violation{
			vType:  policy.UnqualifiedImageViolation,
			reason: UnqualifiedImageReason(image),
		})
		return violations, nil
	}
	// Now, check vulnz in the image
	vulnz, err := metadataFetcher.Vulnerabilities(image)
	if err != nil {
		return nil, err
	}
	maxSev := isp.Spec.PackageVulnerabilityRequirements.MaximumSeverity
	if maxSev == "" {
		maxSev = "CRITICAL"
	}

	maxNoFixSev := isp.Spec.PackageVulnerabilityRequirements.MaximumFixUnavailableSeverity
	if maxNoFixSev == "" {
		maxNoFixSev = "ALLOW_ALL"
	}

	for _, v := range vulnz {
		// First, check if the vulnerability is whitelisted
		if cveInWhitelist(isp, v.CVE) {
			continue
		}

		// Allow operators to set a higher threshold for CVE's that have no fix available.
		if !v.HasFixAvailable {
			ok, err := severityWithinThreshold(maxNoFixSev, v.Severity)
			if err != nil {
				return violations, err
			}
			if ok {
				continue
			}
			violations = append(violations, Violation{
				vulnerability: v,
				vType:         policy.FixUnavailableViolation,
				reason:        FixUnavailableReason(image, v, isp),
			})
			continue
		}
		ok, err := severityWithinThreshold(maxSev, v.Severity)
		if err != nil {
			return violations, err
		}
		if ok {
			continue
		}
		violations = append(violations, Violation{
			vulnerability: v,
			vType:         policy.SeverityViolation,
			reason:        SeverityReason(image, v, isp),
		})
	}

	// Check if image has ArkCI signature
	var signedProjectID string

	occs, err := metadataFetcher.OccurencesV1(image)
	for _, occ := range occs {
		if occ.NoteName == os.Getenv("ARKCI_SIGNATURE_NOTE") {
			b, _ := json.Marshal(occ)
			glog.Infof("ArkCI signature = %v", string(b))

			token, err := verifyArkSignature(context.Background(), occ)
			if err != nil {
				violations = append(
					violations,
					NewViolation(
						nil,
						policy.ArkCISignatureViolation,
						policy.Reason(
							fmt.Sprintf("failed to verify ArkCI signature: %s", err),
						),
					),
				)
				continue
			}

			glog.Info("ArkCI signature verified")
			if claims, ok := token.Claims.(jwt.MapClaims); ok {
				signedProjectID, _ = claims["gcp_project"].(string)
			}
		}
	}

	// Check image namespace against BuiltProjectIDs
	// Previously this was checking against build.Provenance.ProjectID, but that is no longer available
	glog.Infof("isp.Spec.BuiltProjectIDs = %v", isp.Spec.BuiltProjectIDs)
	if len(isp.Spec.BuiltProjectIDs) > 0 {
		hasBuildProjectID := false
		for _, projectID := range isp.Spec.BuiltProjectIDs {
			// imageInGCR should be deprecated in the future, replaced by ArkCI signature
			if projectID == signedProjectID || imageInGCR(projectID, image) {
				hasBuildProjectID = true
				break
			}
		}

		if !hasBuildProjectID {
			violations = append(
				violations,
				NewViolation(
					nil,
					policy.BuildProjectIDViolation,
					policy.Reason(
						fmt.Sprintf(
							"%q doesn't come from a permitted GCR: [%s]",
							image,
							strings.Join(isp.Spec.BuiltProjectIDs, ","),
						),
					),
				),
			)
		}
	}

	// Check required attestations
	glog.Infof("isp.Spec.RequireAttestationsBy = %v", isp.Spec.RequireAttestationsBy)
	if len(isp.Spec.RequireAttestationsBy) > 0 {
		attestations, err := metadataFetcher.Attestations(image)
		if err != nil {
			return nil, err
		}
		for _, required := range isp.Spec.RequireAttestationsBy {
			requiredAttestor, err := attestorFetcher.GetAttestor(required)
			if err != nil {
				return nil, errors.Wrapf(err, "failed to get an attestor: %s", required)
			}
			if requiredAttestor == nil {
				return nil, fmt.Errorf("attestor not found: %s", required)
			}

			ok, err := hasRequiredAttestation(image, requiredAttestor, attestations)
			if err != nil {
				return nil, errors.Wrapf(err, "failed to check if required attestation exist: %s, %s", image, required)
			}
			if !ok {
				violations = append(
					violations,
					NewViolation(
						nil,
						policy.RequiredAttestationViolation,
						policy.Reason(
							fmt.Sprintf(
								"%q doesn't have a required attestation: [%s]",
								image,
								required,
							),
						),
					),
				)
			}
		}
	}

	return violations, nil
}

func verifyArkSignature(ctx context.Context, occ *metadata.OccurenceV1) (*jwt.Token, error) {
	config := &gcpjwt.KMSConfig{
		KeyPath: os.Getenv("ARKCI_KMS_SIGNER_KEY"),
	}

	keyFunc, err := gcpjwt.KMSVerfiyKeyfunc(ctx, config)
	if err != nil {
		return nil, err
	}

	for _, j := range occ.Attestation.Jwts {
		token, err := jwt.Parse(j.CompactJwt, func(token *jwt.Token) (interface{}, error) {
			if token.Method.Alg() != jwt.SigningMethodRS256.Alg() {
				return nil, fmt.Errorf("unexpected signing method: %v", token.Method.Alg())
			}

			// To bypass signing method check in gcpjwt
			token.Method = gcpjwt.SigningMethodKMSRS256

			return keyFunc(token)
		})

		if err != nil {
			return nil, err
		}

		if !token.Valid {
			return nil, fmt.Errorf("token is not valid")
		}

		return token, nil
	}

	return nil, fmt.Errorf("no jwt found")
}

func imageInWhitelist(isp v1beta1.ImageSecurityPolicy, image string) bool {
	for _, i := range isp.Spec.ImageWhitelist {
		if i == image {
			return true
		}
	}
	return false
}

func imageInGCR(projectID string, image string) bool {
	prefixes := []string{
		"gcr.io",
		"asia.gcr.io",
		"eu.gcr.io",
		"us.gcr.io",
	}

	for _, p := range prefixes {
		fullPrefix := fmt.Sprintf("%s/%s/", p, projectID)
		if strings.HasPrefix(image, fullPrefix) {
			return true
		}
	}

	return false
}

func cveInWhitelist(isp v1beta1.ImageSecurityPolicy, cve string) bool {
	for _, w := range isp.Spec.PackageVulnerabilityRequirements.WhitelistCVEs {
		if w == cve {
			return true
		}
	}
	return false
}

func severityWithinThreshold(maxSeverity string, severity string) (bool, error) {
	if maxSeverity == constants.BlockAll {
		return false, nil
	}
	if maxSeverity == constants.AllowAll {
		return true, nil
	}
	if _, ok := vulnerability.Severity_value[maxSeverity]; !ok {
		return false, fmt.Errorf("invalid max severity level: %s", maxSeverity)
	}
	if _, ok := vulnerability.Severity_value[severity]; !ok {
		return false, fmt.Errorf("invalid severity level: %s", severity)
	}
	return vulnerability.Severity_value[severity] <= vulnerability.Severity_value[maxSeverity], nil
}

type Attestor struct {
	Name       string
	PublicKeys []*AttestorPublicKey
}

type AttestorPublicKey struct {
	ID         string // ID = Fingerprint
	AsciiArmor string
}

type AttestorFetcher interface {
	GetAttestor(name string) (*Attestor, error)
}

type binauthzAttestorFetcher struct {
	client binauthz.Client
}

func NewAttestorFetcher() (AttestorFetcher, error) {
	client, err := binauthz.New()
	if err != nil {
		return nil, errors.Wrapf(err, "failed to create a binauthz client")
	}
	return &binauthzAttestorFetcher{
		client: client,
	}, nil
}

func (f *binauthzAttestorFetcher) GetAttestor(name string) (*Attestor, error) {
	a, err := f.client.GetAttestor(context.Background(), name)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get an attestor: %s", name)
	}
	if a.UserOwnedGrafeasNote == nil {
		return nil, errors.Wrapf(err, "attestor doesn't have UserOwnedDrydockNote: %s", name)
	}

	pubKeys := []*AttestorPublicKey{}
	for _, pubKey := range a.UserOwnedGrafeasNote.PublicKeys {
		pubKeys = append(pubKeys, &AttestorPublicKey{
			ID:         pubKey.Id,
			AsciiArmor: pubKey.AsciiArmoredPgpPublicKey,
		})
	}

	attestor := &Attestor{
		Name:       name,
		PublicKeys: pubKeys,
	}

	return attestor, nil
}

func hasRequiredAttestation(image string, attestor *Attestor, attestations []metadata.PGPAttestation) (bool, error) {
	sig, err := container.NewAtomicContainerSig(image, map[string]string{})
	if err != nil {
		return false, errors.Wrapf(err, "failed to initialize attestation signature: %s", image)
	}

	var verified bool
	for _, attestation := range attestations {
		for _, pubKey := range attestor.PublicKeys {
			if pubKey.ID == attestation.KeyID {
				if err := sig.VerifyAttestationSignature(pubKey.AsciiArmor, attestation.Signature); err == nil {
					verified = true
					break
				}
				glog.Warningf("failed to verify attestation signature: KeyID=%s, %v", attestation.KeyID, err)
			}
		}
	}
	return verified, nil
}
