// Copyright 2018 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package provision

import (
	"archive/zip"
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/hex"
	"encoding/pem"
	"encoding/xml"
	"fmt"
	"io"
	"io/ioutil"
	"math/big"
	rnd "math/rand"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/apigee/apigee-remote-service-cli/apigee"
	"github.com/apigee/apigee-remote-service-cli/proxies"
	"github.com/apigee/apigee-remote-service-cli/shared"
	"github.com/apigee/apigee-remote-service-envoy/server"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	"go.uber.org/multierr"
	"gopkg.in/yaml.v3"
)

const ( // legacy
	legacyCredentialURLFormat = "%s/credential/organization/%s/environment/%s"  // InternalProxyURL, org, env
	analyticsURLFormat        = "%s/analytics/organization/%s/environment/%s"   // InternalProxyURL, org, env
	legacyAnalyticURLFormat   = "%s/axpublisher/organization/%s/environment/%s" // InternalProxyURL, org, env
	legacyAuthProxyZip        = "remote-service-legacy.zip"

	// virtualHost is only necessary for legacy
	virtualHostReplaceText    = "<VirtualHost>default</VirtualHost>"
	virtualHostReplacementFmt = "<VirtualHost>%s</VirtualHost>" // each virtualHost

	internalProxyName = "edgemicro-internal" // legacy
	internalProxyZip  = "internal.zip"
)

const ( // modern
	kvmName       = "remote-service"
	cacheName     = "remote-service"
	encryptKVM    = true
	authProxyName = "remote-service"

	remoteServiceProxyZip = "remote-service-gcp.zip"

	apiProductsPath        = "apiproducts"
	developersPath         = "developers"
	applicationsPathFormat = "developers/%s/apps"                // developer email
	keyCreatePathFormat    = "developers/%s/apps/%s/keys/create" // developer email, app ID
	keyPathFormat          = "developers/%s/apps/%s/keys/%s"     // developer email, app ID, key ID

	certsURLFormat        = "%s/certs"        // RemoteServiceProxyURL
	productsURLFormat     = "%s/products"     // RemoteServiceProxyURL
	verifyAPIKeyURLFormat = "%s/verifyApiKey" // RemoteServiceProxyURL
	quotasURLFormat       = "%s/quotas"       // RemoteServiceProxyURL
	rotateURLFormat       = "%s/rotate"       // RemoteServiceProxyURL

	remoteServiceAPIURLFormat = "https://apigee-runtime-%s-%s.%s:8443/remote-service" // org, env, namespace

	fluentdInternalFormat = "apigee-udca-%s-%s.%s:20001" // org, env, namespace
	defaultApigeeCAFile   = "/opt/apigee/tls/ca.crt"
	defaultApigeeCertFile = "/opt/apigee/tls/tls.crt"
	defaultApigeeKeyFile  = "/opt/apigee/tls/tls.key"
)

type provision struct {
	*shared.RootArgs
	certExpirationInYears int
	certKeyStrength       int
	forceProxyInstall     bool
	virtualHosts          string
	verifyOnly            bool
	provisionKey          string
	provisionSecret       string
	developerEmail        string
	namespace             string
}

// Cmd returns base command
func Cmd(rootArgs *shared.RootArgs, printf shared.FormatFn) *cobra.Command {
	p := &provision{RootArgs: rootArgs}

	c := &cobra.Command{
		Use:   "provision",
		Short: "Provision your Apigee environment for remote services",
		Long: `The provision command will set up your Apigee environment for remote services. This includes creating
and installing a remote-service kvm with certificates, creating credentials, and deploying a remote-service proxy
to your organization and environment.`,
		Args: cobra.NoArgs,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			err := rootArgs.Resolve(false, true)
			if err == nil {
				if p.IsGCPManaged && !p.verifyOnly {
					missingFlagNames := []string{}
					if p.Token == "" {
						missingFlagNames = append(missingFlagNames, "token")
					}
					if p.developerEmail == "" {
						missingFlagNames = append(missingFlagNames, "developer-email")
					}
					err = p.PrintMissingFlags(missingFlagNames)
				}
			}
			return err
		},

		RunE: func(cmd *cobra.Command, _ []string) error {
			if p.verifyOnly && (p.provisionKey == "" || p.provisionSecret == "") {
				return fmt.Errorf("--verify-only requires values for --key and --secret")
			}
			return p.run(printf)
		},
	}

	c.Flags().StringVarP(&rootArgs.ManagementBase, "management", "m",
		shared.DefaultManagementBase, "Apigee management base URL")
	c.Flags().BoolVarP(&rootArgs.IsLegacySaaS, "legacy", "", false,
		"Apigee SaaS (sets management and runtime URL)")
	c.Flags().BoolVarP(&rootArgs.IsOPDK, "opdk", "", false,
		"Apigee opdk")

	c.Flags().StringVarP(&rootArgs.Token, "token", "t", "",
		"Apigee OAuth or SAML token (hybrid only)")
	c.Flags().StringVarP(&rootArgs.Username, "username", "u", "",
		"Apigee username (legacy or OPDK only)")
	c.Flags().StringVarP(&rootArgs.Password, "password", "p", "",
		"Apigee password (legacy or OPDK only)")

	c.Flags().StringVarP(&p.developerEmail, "developer-email", "d", "",
		"email used to create a developer (ignored for --legacy or --opdk)")
	c.Flags().IntVarP(&p.certExpirationInYears, "years", "", 1,
		"number of years before the jwt cert expires")
	c.Flags().IntVarP(&p.certKeyStrength, "strength", "", 2048,
		"key strength")
	c.Flags().BoolVarP(&p.forceProxyInstall, "force-proxy-install", "f", false,
		"force new proxy install (upgrades proxy)")
	c.Flags().StringVarP(&p.virtualHosts, "virtual-hosts", "", "default,secure",
		"override proxy virtualHosts")
	c.Flags().BoolVarP(&p.verifyOnly, "verify-only", "", false,
		"verify only, don’t provision anything")
	c.Flags().StringVarP(&p.namespace, "namespace", "n", "",
		"emit configuration as an Envoy ConfigMap in the specified namespace")

	c.Flags().StringVarP(&p.provisionKey, "key", "k", "", "gateway key (for --verify-only)")
	c.Flags().StringVarP(&p.provisionSecret, "secret", "s", "", "gateway secret (for --verify-only)")

	return c
}

func (p *provision) run(printf shared.FormatFn) error {

	var cred *credential

	var verbosef = shared.NoPrintf
	if p.Verbose || p.verifyOnly {
		verbosef = printf
	}

	if !p.verifyOnly {

		tempDir, err := ioutil.TempDir("", "apigee")
		if err != nil {
			return errors.Wrap(err, "creating temp dir")
		}
		defer os.RemoveAll(tempDir)

		replaceVH := func(proxyDir string) error {
			proxiesFile := filepath.Join(proxyDir, "proxies", "default.xml")
			bytes, err := ioutil.ReadFile(proxiesFile)
			if err != nil {
				return errors.Wrapf(err, "reading file %s", proxiesFile)
			}
			newVH := ""
			for _, vh := range strings.Split(p.virtualHosts, ",") {
				if strings.TrimSpace(vh) != "" {
					newVH = newVH + fmt.Sprintf(virtualHostReplacementFmt, vh)
				}
			}
			bytes = []byte(strings.Replace(string(bytes), virtualHostReplaceText, newVH, 1))
			if err := ioutil.WriteFile(proxiesFile, bytes, 0); err != nil {
				return errors.Wrapf(err, "writing file %s", proxiesFile)
			}
			return nil
		}

		replaceInFile := func(file, old, new string) error {
			bytes, err := ioutil.ReadFile(file)
			if err != nil {
				return errors.Wrapf(err, "reading file %s", file)
			}
			bytes = []byte(strings.Replace(string(bytes), old, new, 1))
			if err := ioutil.WriteFile(file, bytes, 0); err != nil {
				return errors.Wrapf(err, "writing file %s", file)
			}
			return nil
		}

		replaceVHAndAuthTarget := func(proxyDir string) error {
			if err := replaceVH(proxyDir); err != nil {
				return err
			}

			if p.IsOPDK {
				// OPDK must target local internal proxy
				authFile := filepath.Join(proxyDir, "policies", "Authenticate-Call.xml")
				oldTarget := "https://edgemicroservices.apigee.net"
				newTarget := p.RuntimeBase
				if err := replaceInFile(authFile, oldTarget, newTarget); err != nil {
					return err
				}

				// OPDK must have org.noncps = true for products callout
				calloutFile := filepath.Join(proxyDir, "policies", "JavaCallout.xml")
				oldValue := "</Properties>"
				newValue := `<Property name="org.noncps">true</Property>
				</Properties>`
				if err := replaceInFile(calloutFile, oldValue, newValue); err != nil {
					return err
				}
			}
			return nil
		}

		if p.IsOPDK {
			if err := p.deployInternalProxy(replaceVH, tempDir, verbosef); err != nil {
				return errors.Wrap(err, "deploying internal proxy")
			}
		}

		// input remote-service proxy
		var customizedProxy string
		if p.IsGCPManaged {
			customizedProxy, err = getCustomizedProxy(tempDir, remoteServiceProxyZip, nil)
		} else {
			customizedProxy, err = getCustomizedProxy(tempDir, legacyAuthProxyZip, replaceVHAndAuthTarget)
		}
		if err != nil {
			return err
		}

		if err := p.checkAndDeployProxy(authProxyName, customizedProxy, verbosef); err != nil {
			return errors.Wrapf(err, "deploying proxy %s", authProxyName)
		}

		if p.IsGCPManaged {
			cred, err = p.createGCPCredential(verbosef)
		} else {
			cred, err = p.createLegacyCredential(verbosef)
		}
		if err != nil {
			return errors.Wrapf(err, "generating credential")
		}

		if !p.IsGCPManaged {
			if err := p.getOrCreateKVM(cred, verbosef); err != nil {
				return errors.Wrapf(err, "retrieving or creating kvm")
			}
		}

	} else { // verifyOnly == true
		cred = &credential{
			Key:    p.provisionKey,
			Secret: p.provisionSecret,
		}
	}

	// use generated credentials
	opts := *p.ClientOpts
	if cred != nil {
		opts.Auth = &apigee.EdgeAuth{
			Username: cred.Key,
			Password: cred.Secret,
		}
		var err error
		if p.Client, err = apigee.NewEdgeClient(&opts); err != nil {
			return errors.Wrapf(err, "creating new client")
		}
	}

	var verifyErrors error
	if p.IsLegacySaaS || p.IsOPDK {
		verbosef("verifying internal proxy...")
		verifyErrors = p.verifyInternalProxy(opts.Auth, verbosef)
	}

	verbosef("verifying remote-service proxy...")
	verifyErrors = multierr.Combine(verifyErrors, p.verifyRemoteServiceProxy(opts.Auth, verbosef))

	if verifyErrors != nil {
		shared.Errorf("\nWARNING: Apigee may not be provisioned properly.")
		shared.Errorf("Unable to verify proxy endpoint(s). Errors:\n")
		for _, err := range multierr.Errors(verifyErrors) {
			shared.Errorf("  %s", err)
		}
		shared.Errorf("\n")
	}

	if !p.verifyOnly {
		if err := p.printConfig(cred, printf, verifyErrors); err != nil {
			return errors.Wrapf(err, "generating config")
		}
	}

	if verifyErrors != nil {
		os.Exit(1)
	}

	verbosef("provisioning verified OK")
	return nil
}

// ensures that there's a product, developer, and app
func (p *provision) createGCPCredential(verbosef shared.FormatFn) (*credential, error) {
	const removeServiceName = "remote-service"

	// create product
	product := apiProduct{
		Name:         removeServiceName,
		DisplayName:  removeServiceName,
		ApprovalType: "auto",
		Attributes: []attribute{
			{Name: "access", Value: "internal"},
		},
		Description:  removeServiceName + " access",
		APIResources: []string{"/**"},
		Environments: []string{p.Env},
		Proxies:      []string{removeServiceName},
	}
	req, err := p.Client.NewRequestNoEnv(http.MethodPost, apiProductsPath, product)
	if err != nil {
		return nil, err
	}
	res, err := p.Client.Do(req, nil)
	if err != nil {
		if res.StatusCode != http.StatusConflict { // exists
			return nil, err
		}
		verbosef("product %s already exists", removeServiceName)
	}

	// create developer
	devEmail := p.developerEmail
	dev := developer{
		Email:     devEmail,
		FirstName: removeServiceName,
		LastName:  removeServiceName,
		UserName:  removeServiceName,
	}
	req, err = p.Client.NewRequestNoEnv(http.MethodPost, developersPath, dev)
	if err != nil {
		return nil, err
	}
	res, err = p.Client.Do(req, nil)
	if err != nil {
		if res.StatusCode != http.StatusConflict { // exists
			return nil, err
		}
		verbosef("developer %s already exists", devEmail)
	}

	// create application
	app := application{
		Name:        removeServiceName,
		APIProducts: []string{removeServiceName},
	}
	applicationsPath := fmt.Sprintf(applicationsPathFormat, devEmail)
	req, err = p.Client.NewRequestNoEnv(http.MethodPost, applicationsPath, &app)
	if err != nil {
		return nil, err
	}
	res, err = p.Client.Do(req, &app)
	if err == nil {
		appCred := app.Credentials[0]
		cred := &credential{
			Key:    appCred.Key,
			Secret: appCred.Secret,
		}
		verbosef("credentials created: %v", cred)
		return cred, nil
	}

	if res == nil || res.StatusCode != http.StatusConflict {
		return nil, err
	}

	// http.StatusConflict == app exists, create a new credential
	verbosef("app %s already exists", removeServiceName)
	appCred := appCredential{
		Key:    newHash(),
		Secret: newHash(),
	}
	createKeyPath := fmt.Sprintf(keyCreatePathFormat, devEmail, removeServiceName)
	if req, err = p.Client.NewRequestNoEnv(http.MethodPost, createKeyPath, &appCred); err != nil {
		return nil, err
	}
	if res, err = p.Client.Do(req, &appCred); err != nil {
		return nil, err
	}

	// adding product to the credential requires a separate call
	appCredDetails := appCredentialDetails{
		APIProducts: []string{removeServiceName},
	}
	keyPath := fmt.Sprintf(keyPathFormat, devEmail, removeServiceName, appCred.Key)
	if req, err = p.Client.NewRequestNoEnv(http.MethodPost, keyPath, &appCredDetails); err != nil {
		return nil, err
	}
	if res, err = p.Client.Do(req, &appCred); err != nil {
		return nil, err
	}

	cred := &credential{
		Key:    appCred.Key,
		Secret: appCred.Secret,
	}
	verbosef("credentials created: %v", cred)

	return cred, nil
}

func (p *provision) deployInternalProxy(replaceVirtualHosts func(proxyDir string) error, tempDir string, verbosef shared.FormatFn) error {

	customizedZip, err := getCustomizedProxy(tempDir, internalProxyZip, func(proxyDir string) error {

		// change server locations
		calloutFile := filepath.Join(proxyDir, "policies", "Callout.xml")
		bytes, err := ioutil.ReadFile(calloutFile)
		if err != nil {
			return errors.Wrapf(err, "reading file %s", calloutFile)
		}
		var callout JavaCallout
		if err := xml.Unmarshal(bytes, &callout); err != nil {
			return errors.Wrapf(err, "unmarshalling %s", calloutFile)
		}
		setMgmtURL := false
		for i, cp := range callout.Properties {
			if cp.Name == "REGION_MAP" {
				callout.Properties[i].Value = fmt.Sprintf("DN=%s", p.RuntimeBase)
			}
			if cp.Name == "MGMT_URL_PREFIX" {
				setMgmtURL = true
				callout.Properties[i].Value = p.ManagementBase
			}
		}
		if !setMgmtURL {
			callout.Properties = append(callout.Properties,
				javaCalloutProperty{
					Name:  "MGMT_URL_PREFIX",
					Value: p.ManagementBase,
				})
		}

		writer, err := os.OpenFile(calloutFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0)
		if err != nil {
			return errors.Wrapf(err, "writing file %s", calloutFile)
		}
		writer.WriteString(xml.Header)
		encoder := xml.NewEncoder(writer)
		encoder.Indent("", "  ")
		err = encoder.Encode(callout)
		if err != nil {
			return errors.Wrapf(err, "encoding xml to %s", calloutFile)
		}
		err = writer.Close()
		if err != nil {
			return errors.Wrapf(err, "closing file %s", calloutFile)
		}

		return replaceVirtualHosts(proxyDir)
	})
	if err != nil {
		return err
	}

	return p.checkAndDeployProxy(internalProxyName, customizedZip, verbosef)
}

type proxyModFunc func(name string) error

// returns filename of zipped proxy
func getCustomizedProxy(tempDir, name string, modFunc proxyModFunc) (string, error) {
	if err := proxies.RestoreAsset(tempDir, name); err != nil {
		return "", errors.Wrapf(err, "restoring asset %s", name)
	}
	zipFile := filepath.Join(tempDir, name)
	if modFunc == nil {
		return zipFile, nil
	}

	extractDir, err := ioutil.TempDir(tempDir, "proxy")
	if err != nil {
		return "", errors.Wrap(err, "creating temp dir")
	}
	if err := unzipFile(zipFile, extractDir); err != nil {
		return "", errors.Wrapf(err, "extracting %s to %s", zipFile, extractDir)
	}

	if err := modFunc(filepath.Join(extractDir, "apiproxy")); err != nil {
		return "", err
	}

	// write zip
	customizedZip := filepath.Join(tempDir, "customized.zip")
	if err := zipDir(extractDir, customizedZip); err != nil {
		return "", errors.Wrapf(err, "zipping dir %s to file %s", extractDir, customizedZip)
	}

	return customizedZip, nil
}

// hash for key and secret
func newHash() string {
	// use crypto seed
	var seed int64
	binary.Read(rand.Reader, binary.BigEndian, &seed)
	rnd.Seed(seed)

	t := time.Now()
	h := sha256.New()
	h.Write([]byte(t.String() + string(rnd.Int())))
	str := hex.EncodeToString(h.Sum(nil))
	return str
}

// GenKeyCert generates a self signed key and certificate
// returns certBytes, privateKeyBytes, error
func GenKeyCert(keyStrength, certExpirationInYears int) (string, string, error) {
	privateKey, err := rsa.GenerateKey(rand.Reader, keyStrength)
	if err != nil {
		return "", "", errors.Wrap(err, "generating private key")
	}
	now := time.Now()
	subKeyIDHash := sha256.New()
	_, err = subKeyIDHash.Write(privateKey.N.Bytes())
	if err != nil {
		return "", "", errors.Wrap(err, "generating key id")
	}
	subKeyID := subKeyIDHash.Sum(nil)
	template := x509.Certificate{
		SerialNumber: new(big.Int).SetInt64(0),
		Subject: pkix.Name{
			CommonName:   kvmName,
			Organization: []string{kvmName},
		},
		NotBefore:    now.Add(-5 * time.Minute).UTC(),
		NotAfter:     now.AddDate(certExpirationInYears, 0, 0).UTC(),
		IsCA:         true,
		SubjectKeyId: subKeyID,
		KeyUsage: x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature |
			x509.KeyUsageDataEncipherment,
	}
	derBytes, err := x509.CreateCertificate(
		rand.Reader, &template, &template, &privateKey.PublicKey, privateKey)
	if err != nil {
		return "", "", errors.Wrap(err, "creating CA certificate")
	}

	certBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})

	keyBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(privateKey)})

	return string(certBytes), string(keyBytes), nil
}

//check if the KVM exists, if it doesn't, create a new one and sets certs for JWT
func (p *provision) getOrCreateKVM(cred *credential, printf shared.FormatFn) error {

	cert, privateKey, err := GenKeyCert(p.certKeyStrength, p.certExpirationInYears)
	if err != nil {
		return err
	}

	kvm := apigee.KVM{
		Name:      kvmName,
		Encrypted: encryptKVM,
	}

	if !p.IsGCPManaged { // GCP API breaks with any initial entries
		kvm.Entries = []apigee.Entry{
			{
				Name:  "private_key",
				Value: privateKey,
			},
			{
				Name:  "certificate1",
				Value: cert,
			},
			{
				Name:  "certificate1_kid",
				Value: "1",
			},
		}
	}

	resp, err := p.Client.KVMService.Create(kvm)
	if err != nil && (resp == nil || resp.StatusCode != http.StatusConflict) { // http.StatusConflict == already exists
		return err
	}
	if resp.StatusCode == http.StatusConflict {
		printf("kvm %s already exists", kvmName)
		return nil
	}
	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("creating kvm %s, status code: %v", kvmName, resp.StatusCode)
	}
	printf("kvm %s created", kvmName)

	printf("registered a new key and cert for JWTs:\n")
	printf("certificate:\n%s", cert)
	printf("private key:\n%s", privateKey)

	return nil
}

func (p *provision) createLegacyCredential(printf shared.FormatFn) (*credential, error) {
	printf("creating credential...")
	cred := &credential{
		Key:    newHash(),
		Secret: newHash(),
	}

	credentialURL := fmt.Sprintf(legacyCredentialURLFormat, p.InternalProxyURL, p.Org, p.Env)

	req, err := p.Client.NewRequest(http.MethodPost, credentialURL, cred)
	if err != nil {
		return nil, err
	}
	req.URL, err = url.Parse(credentialURL) // override client's munged URL
	if err != nil {
		return nil, err
	}

	resp, err := p.Client.Do(req, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode > 299 {
		return nil, fmt.Errorf("creating credential, status: %d", resp.StatusCode)
	}
	printf("credential created")
	return cred, nil
}

/*
# Example ConfigMap for apigee-remote-service-envoy configuration in SaaS.
# You must update the tenant values appropriately.
# Note: Alternatively, you can use the generated config from the CLI directly.
#       Direct the output to `config.yaml` and run the follow command on it:
#       `kubectl -n apigee create configmap apigee-remote-service-envoy --from-file=config.yaml`
apiVersion: v1
kind: ConfigMap
metadata:
  name: apigee-remote-service-envoy
  namespace: apigee
data:
	config.yaml: |
		xxxx...
*/
func (p *provision) printConfig(cred *credential, printf shared.FormatFn, verifyErrors error) error {

	config := server.Config{
		Tenant: server.TenantConfig{
			InternalAPI:      p.InternalProxyURL,
			RemoteServiceAPI: p.RemoteServiceProxyURL,
			OrgName:          p.Org,
			EnvName:          p.Env,
			Key:              cred.Key,
			Secret:           cred.Secret,
		},
	}

	if p.IsGCPManaged {
		config.Tenant.InternalAPI = "" // no internal API for GCP
		config.Analytics.CollectionInterval = 10 * time.Second

		// assumes the same mesh and tls files are mounted properly
		fluentdNS := p.namespace
		if fluentdNS == "" {
			fluentdNS = "apigee"
		}
		config.Analytics.FluentdEndpoint = fmt.Sprintf(fluentdInternalFormat, p.Org, p.Env, fluentdNS)
		config.Analytics.TLS.CAFile = defaultApigeeCAFile
		config.Analytics.TLS.CertFile = defaultApigeeCertFile
		config.Analytics.TLS.KeyFile = defaultApigeeKeyFile
	}

	if p.IsOPDK {
		config.Analytics.LegacyEndpoint = true
	}

	// encode config
	var yamlBuffer bytes.Buffer
	yamlEncoder := yaml.NewEncoder(&yamlBuffer)
	yamlEncoder.SetIndent(2)
	err := yamlEncoder.Encode(config)
	if err != nil {
		return err
	}
	configYAML := yamlBuffer.String()

	print := func(config string) error {
		printf("# Configuration for apigee-remote-service-envoy")
		printf("# generated by apigee-remote-service-cli provision on %s", time.Now().Format("2006-01-02 15:04:05"))
		if verifyErrors != nil {
			printf("# WARNING: verification of provision failed. May not be valid.")
		}
		printf(config)
		return nil
	}

	if p.namespace == "" {
		return print(configYAML)
	}

	// ConfigMap
	data := map[string]string{"config.yaml": configYAML}
	crd := shared.KubernetesCRD{
		APIVersion: "v1",
		Kind:       "ConfigMap",
		Metadata: shared.Metadata{
			Name:      "apigee-remote-service-envoy",
			Namespace: p.namespace,
		},
		Data: data,
	}

	yamlBuffer.Reset()
	yamlEncoder = yaml.NewEncoder(&yamlBuffer)
	yamlEncoder.SetIndent(2)
	err = yamlEncoder.Encode(crd)
	if err != nil {
		return err
	}
	configMapYAML := yamlBuffer.String()

	return print(configMapYAML)
}

func (p *provision) checkAndDeployProxy(name, file string, printf shared.FormatFn) error {
	printf("checking if proxy %s deployment exists...", name)
	var oldRev *apigee.Revision
	var err error
	if p.IsGCPManaged {
		oldRev, err = p.Client.Proxies.GetGCPDeployedRevision(name)
	} else {
		oldRev, err = p.Client.Proxies.GetDeployedRevision(name)
	}
	if err != nil {
		return err
	}
	if oldRev != nil {
		if p.forceProxyInstall {
			printf("replacing proxy %s revision %s in %s", name, oldRev, p.Env)
		} else {
			printf("proxy %s revision %s already deployed to %s", name, oldRev, p.Env)
			return nil
		}
	}

	printf("checking proxy %s status...", name)
	var resp *apigee.Response
	proxy, resp, err := p.Client.Proxies.Get(name)
	if err != nil && (resp == nil || resp.StatusCode != 404) {
		return err
	}

	return p.importAndDeployProxy(name, proxy, oldRev, file, printf)
}

func (p *provision) importAndDeployProxy(name string, proxy *apigee.Proxy, oldRev *apigee.Revision, file string, printf shared.FormatFn) error {
	var newRev apigee.Revision = 1
	if proxy != nil && len(proxy.Revisions) > 0 {
		sort.Sort(apigee.RevisionSlice(proxy.Revisions))
		newRev = proxy.Revisions[len(proxy.Revisions)-1] + 1
		printf("proxy %s exists. highest revision is: %d", name, newRev-1)
	}

	// create a new client to avoid dumping the proxy binary to stdout during Import
	noDebugClient := p.Client
	if p.Verbose {
		opts := *p.ClientOpts
		opts.Debug = false
		var err error
		noDebugClient, err = apigee.NewEdgeClient(&opts)
		if err != nil {
			return err
		}
	}

	printf("creating new proxy %s revision: %d...", name, newRev)
	_, res, err := noDebugClient.Proxies.Import(name, file)
	if res != nil {
		defer res.Body.Close()
	}
	if err != nil {
		return errors.Wrapf(err, "importing proxy %s", name)
	}

	if oldRev != nil && !p.IsGCPManaged { // it's not necessary to undeploy first with GCP
		printf("undeploying proxy %s revision %d on env %s...",
			name, oldRev, p.Env)
		_, res, err = p.Client.Proxies.Undeploy(name, p.Env, *oldRev)
		if res != nil {
			defer res.Body.Close()
		}
		if err != nil {
			return errors.Wrapf(err, "undeploying proxy %s", name)
		}
	}

	if !p.IsGCPManaged {
		cache := apigee.Cache{
			Name: cacheName,
		}
		res, err = p.Client.CacheService.Create(cache)
		if err != nil && (res == nil || res.StatusCode != http.StatusConflict) { // http.StatusConflict == already exists
			return err
		}
		if res.StatusCode != http.StatusCreated && res.StatusCode != http.StatusConflict {
			return fmt.Errorf("creating cache %s, status code: %v", cacheName, res.StatusCode)
		}
		if res.StatusCode == http.StatusConflict {
			printf("cache %s already exists", cacheName)
		} else {
			printf("cache %s created", cacheName)
		}
	}

	printf("deploying proxy %s revision %d to env %s...", name, newRev, p.Env)
	_, res, err = p.Client.Proxies.Deploy(name, p.Env, newRev)
	if res != nil {
		defer res.Body.Close()
	}
	if err != nil {
		return errors.Wrapf(err, "deploying proxy %s", name)
	}

	return nil
}

// verify POST internalProxyURL/analytics/organization/%s/environment/%s
// verify POST internalProxyURL/quotas/organization/%s/environment/%s
func (p *provision) verifyInternalProxy(auth *apigee.EdgeAuth, printf shared.FormatFn) error {
	var verifyErrors error

	var req *http.Request
	var err error
	var res *apigee.Response
	if p.IsOPDK {
		analyticsURL := fmt.Sprintf(legacyAnalyticURLFormat, p.InternalProxyURL, p.Org, p.Env)
		req, err = http.NewRequest(http.MethodPost, analyticsURL, strings.NewReader("{}"))
	} else {
		analyticsURL := fmt.Sprintf(analyticsURLFormat, p.InternalProxyURL, p.Org, p.Env)
		req, err = http.NewRequest(http.MethodGet, analyticsURL, nil)
		q := req.URL.Query()
		q.Add("tenant", fmt.Sprintf("%s~%s", p.Org, p.Env))
		q.Add("relative_file_path", "fake")
		q.Add("file_content_type", "application/x-gzip")
		q.Add("encrypt", "true")
		req.URL.RawQuery = q.Encode()
	}
	if err != nil {
		auth.ApplyTo(req)
		res, err = p.Client.Do(req, nil)
		if res != nil {
			defer res.Body.Close()
		}
	}
	if err != nil {
		verifyErrors = multierr.Append(verifyErrors, err)
	}

	return verifyErrors
}

// verify GET RemoteServiceProxyURL/certs
// verify GET RemoteServiceProxyURL/products
// verify POST RemoteServiceProxyURL/verifyApiKey
// verify POST RemoteServiceProxyURL/quotas
func (p *provision) verifyRemoteServiceProxy(auth *apigee.EdgeAuth, printf shared.FormatFn) error {

	verifyGET := func(targetURL string) error {
		req, err := http.NewRequest(http.MethodGet, targetURL, nil)
		if err != nil {
			return errors.Wrapf(err, "creating request")
		}
		auth.ApplyTo(req)
		res, err := p.Client.Do(req, nil)
		if res != nil {
			defer res.Body.Close()
		}
		return err
	}

	var res *apigee.Response
	var verifyErrors error
	certsURL := fmt.Sprintf(certsURLFormat, p.RemoteServiceProxyURL)
	err := verifyGET(certsURL)
	verifyErrors = multierr.Append(verifyErrors, err)

	productsURL := fmt.Sprintf(productsURLFormat, p.RemoteServiceProxyURL)
	err = verifyGET(productsURL)
	verifyErrors = multierr.Append(verifyErrors, err)

	verifyAPIKeyURL := fmt.Sprintf(verifyAPIKeyURLFormat, p.RemoteServiceProxyURL)
	body := fmt.Sprintf(`{ "apiKey": "%s" }`, auth.Username)
	req, err := http.NewRequest(http.MethodPost, verifyAPIKeyURL, strings.NewReader(body))
	if err == nil {
		req.Header.Add("Content-Type", "application/json")
		auth.ApplyTo(req)
		res, err = p.Client.Do(req, nil)
		if res != nil {
			defer res.Body.Close()
		}
	}
	if err != nil && (res == nil || res.StatusCode != 401) { // 401 is ok, we don't actually have a valid api key to test
		verifyErrors = multierr.Append(verifyErrors, err)
	}

	quotasURL := fmt.Sprintf(quotasURLFormat, p.RemoteServiceProxyURL)
	req, err = http.NewRequest(http.MethodPost, quotasURL, strings.NewReader("{}"))
	if err == nil {
		req.Header.Add("Content-Type", "application/json")
		auth.ApplyTo(req)
		res, err = p.Client.Do(req, nil)
		if res != nil {
			defer res.Body.Close()
		}
	}
	if err != nil {
		verifyErrors = multierr.Append(verifyErrors, err)
	}

	return verifyErrors
}

func unzipFile(src, dest string) error {
	r, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer r.Close()

	os.MkdirAll(dest, 0755)

	extract := func(f *zip.File) error {
		rc, err := f.Open()
		if err != nil {
			return err
		}
		defer rc.Close()

		path := filepath.Join(dest, f.Name)

		if f.FileInfo().IsDir() {
			os.MkdirAll(path, f.Mode())
		} else {
			os.MkdirAll(filepath.Dir(path), f.Mode())
			f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, f.Mode())
			if err != nil {
				return err
			}
			defer f.Close()

			_, err = io.Copy(f, rc)
			if err != nil {
				return err
			}
		}
		return nil
	}

	for _, f := range r.File {
		err := extract(f)
		if err != nil {
			return err
		}
	}

	return nil
}

func zipDir(source, file string) error {
	zipFile, err := os.Create(file)
	if err != nil {
		return err
	}
	defer zipFile.Close()

	w := zip.NewWriter(zipFile)

	var addFiles func(w *zip.Writer, fileBase, zipBase string) error
	addFiles = func(w *zip.Writer, fileBase, zipBase string) error {
		files, err := ioutil.ReadDir(fileBase)
		if err != nil {
			return err
		}

		for _, file := range files {
			fqName := filepath.Join(fileBase, file.Name())
			zipFQName := filepath.Join(zipBase, file.Name())

			if file.IsDir() {
				addFiles(w, fqName, zipFQName)
				continue
			}

			bytes, err := ioutil.ReadFile(fqName)
			if err != nil {
				return err
			}
			f, err := w.Create(zipFQName)
			if err != nil {
				return err
			}
			if _, err = f.Write(bytes); err != nil {
				return err
			}
		}
		return nil
	}

	err = addFiles(w, source, "")
	if err != nil {
		return err
	}

	return w.Close()
}

type credential struct {
	Key    string `json:"key"`
	Secret string `json:"secret"`
}

// JavaCallout must be capitalized to ensure correct generation
type JavaCallout struct {
	Name                                string `xml:"name,attr"`
	DisplayName, ClassName, ResourceURL string
	Properties                          []javaCalloutProperty `xml:"Properties>Property"`
}

type javaCalloutProperty struct {
	Name  string `xml:"name,attr"`
	Value string `xml:",chardata"`
}

type connection struct {
	Address string `yaml:"address"`
}

type apiProduct struct {
	Name         string      `json:"name,omitempty"`
	DisplayName  string      `json:"displayName,omitempty"`
	ApprovalType string      `json:"approvalType,omitempty"`
	Attributes   []attribute `json:"attributes,omitempty"`
	Description  string      `json:"description,omitempty"`
	APIResources []string    `json:"apiResources,omitempty"`
	Environments []string    `json:"environments,omitempty"`
	Proxies      []string    `json:"proxies,omitempty"`
}

type attribute struct {
	Name  string `json:"name,omitempty"`
	Value string `json:"value,omitempty"`
}

type developer struct {
	Email     string `json:"email,omitempty"`
	FirstName string `json:"firstName,omitempty"`
	LastName  string `json:"lastName,omitempty"`
	UserName  string `json:"userName,omitempty"`
}

type application struct {
	Name        string          `json:"name,omitempty"`
	APIProducts []string        `json:"apiProducts,omitempty"`
	Credentials []appCredential `json:"credentials,omitempty"`
}

type appCredential struct {
	Key    string `json:"consumerKey,omitempty"`
	Secret string `json:"consumerSecret,omitempty"`
}

type rotateRequest struct {
	PrivateKey  string `json:"private_key"`
	Certificate string `json:"certificate"`
	KeyID       string `json:"kid"`
}

type appCredentialDetails struct {
	APIProducts []string    `json:"apiProducts,omitempty"`
	Attributes  []attribute `json:"attributes,omitempty"`
}
