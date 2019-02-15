package main

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"strings"

	"crypto/tls"

	"context"

	"bytes"
	"encoding/json"

	"github.com/Azure/azure-sdk-for-go/services/keyvault/v7.0/keyvault"
	"github.com/Azure/go-autorest/autorest"
	"github.com/cosmincojocar/adal"
	"github.com/namsral/flag"
	"golang.org/x/crypto/pkcs12"
)

const (
	activeDirectoryEndpoint = "https://login.microsoftonline.com/"
	resource                = "https://vault.azure.net"
	akePrefix               = "AKE"
)

type option struct {
	name  string
	value string
}

var (
	vaultName       string
	tenantID        string
	applicationID   string
	certificatePath string
	servicePrefix   string
	jsonOutput      bool
)

func init() {

	fs := flag.NewFlagSetWithEnvPrefix(os.Args[0], akePrefix, 0)

	fs.StringVar(&vaultName, "vaultName", "", "key vault from which secrets are retrieved")
	fs.StringVar(&tenantID, "tenantId", "", "tenant id")
	fs.StringVar(&applicationID, "applicationId", "", "application id for service principal")
	fs.StringVar(&certificatePath, "certificatePath", "", "path to pk12/PFC application certificate")
	fs.StringVar(&servicePrefix, "servicePrefix", "", "prefix to filter keys for service")
	fs.BoolVar(&jsonOutput, "json", false, "json output")

	fs.Parse(os.Args[1:])

	checkMandatoryOptions(
		option{name: "vaultName", value: vaultName},
		option{name: "tenantId", value: tenantID},
		option{name: "applicationId", value: applicationID},
		option{name: "certificatePath", value: certificatePath},
		option{name: "servicePrefix", value: servicePrefix},
	)
}

func main() {
	oauthConfig, err := adal.NewOAuthConfig(activeDirectoryEndpoint, tenantID)
	if err != nil {
		log.Fatalf("Failed to create OAuth config: %q", err)
	}

	spt, err := acquireTokenClientCertFlow(
		*oauthConfig,
		applicationID,
		certificatePath,
		resource)

	if err != nil {
		log.Fatalf("Failed to acquire a token for resource %s. Error: %v", resource, err)
	}

	envVars, err := expandVars(vaultName, spt)
	if err != nil {
		log.Fatalf("Failed to expand environment: %v", err)
	}

	if jsonOutput {
		output, _ := json.Marshal(envVars)
		fmt.Println(string(output))
	} else {
		for key, value := range envVars {
			exportString := fmt.Sprintf("export %s=\"%s\"", key, value)
			fmt.Println(exportString)
		}
	}
}

func checkMandatoryOptions(options ...option) {
	for _, option := range options {
		if strings.TrimSpace(option.value) == "" {
			log.Fatalf("Authentication requires mandatory option '%s'.", option.name)
		}
	}
}

func decodePkcs12(pkcs []byte, password string) (*x509.Certificate, *rsa.PrivateKey, error) {
	privateKey, certificate, err := pkcs12.Decode(pkcs, password)
	if err != nil {
		return nil, nil, err
	}

	rsaPrivateKey, isRsaKey := privateKey.(*rsa.PrivateKey)
	if !isRsaKey {
		return nil, nil, fmt.Errorf("PKCS#12 certificate must contain an RSA private key")
	}

	return certificate, rsaPrivateKey, nil
}

func decodePem(filePath string) (*x509.Certificate, *rsa.PrivateKey, error) {

	pemFile, err := ioutil.ReadFile(filePath)

	cliCertIndex := bytes.Index(pemFile, []byte("issuer=/CN=CLI-Login"))
	if cliCertIndex == 0 {
		return nil, nil, fmt.Errorf("couldn't find CLI-Login certificate")
	}
	cliKeyID := pemFile[cliCertIndex-41 : cliCertIndex-23]
	cliCertificate := pemFile[cliCertIndex+20:]

	cliCertEnd := bytes.Index(cliCertificate, []byte("-----END CERTIFICATE-----"))
	if cliCertEnd == 0 {
		return nil, nil, fmt.Errorf("couldn't parse CLI-Login certificate")
	}
	cliCertificate = cliCertificate[:cliCertEnd+25]

	cliPrivateIndex := bytes.Index(pemFile, cliKeyID)
	if cliCertIndex == 0 {
		return nil, nil, fmt.Errorf("couldn't find CLI-Login private key")
	}
	cliPrivateKey := pemFile[cliPrivateIndex:]
	cliPrivateEnd := bytes.Index(cliPrivateKey, []byte("-----END PRIVATE KEY-----"))
	if cliPrivateEnd == 0 {
		return nil, nil, fmt.Errorf("couldn't parse CLI-Login private key")
	}
	cliPrivateKey = cliPrivateKey[:cliPrivateEnd+25]

	cert, err := tls.X509KeyPair(cliCertificate, cliPrivateKey)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to load x509 keypair: %q", err)
	}
	x509Cert, err := x509.ParseCertificate(cert.Certificate[0])
	key, ok := cert.PrivateKey.(*rsa.PrivateKey)
	if !ok {
		return nil, nil, fmt.Errorf("incorrect private key %v", cert.PrivateKey)
	}
	return x509Cert, key, err
}

func acquireTokenClientCertFlow(oauthConfig adal.OAuthConfig,
	applicationID string,
	applicationCertPath string,
	resource string) (*adal.ServicePrincipalToken, error) {
	var rsaPrivateKey *rsa.PrivateKey
	var certificate *x509.Certificate
	var err error

	if strings.HasSuffix(certificatePath, ".pfx") {
		certData, err := ioutil.ReadFile(certificatePath)
		if err != nil {
			return nil, fmt.Errorf("failed to read the certificate file (%s): %v", certificatePath, err)
		}
		certificate, rsaPrivateKey, err = decodePkcs12(certData, "")
	} else {
		certificate, rsaPrivateKey, err = decodePem(certificatePath)
	}

	if err != nil {
		return nil, fmt.Errorf("failed to decode certificate and private key while creating spt: %v", err)
	}

	spt, err := adal.NewServicePrincipalTokenFromCertificate(
		oauthConfig,
		applicationID,
		certificate,
		rsaPrivateKey,
		resource)

	if err != nil {
		return nil, err
	}

	return spt, spt.Refresh()
}

func splitVar(v string) (key, val string) {
	parts := strings.Split(v, "=")
	return parts[0], parts[1]
}

func expandVars(vaultName string,
	spt *adal.ServicePrincipalToken) (map[string]string, error) {
	var exportStrings []string
	var secretName string
	var secretDest string
	var secretFile bool
	exportVars := make(map[string]string)

	vaultClient := keyvault.New()
	vaultClient.Authorizer = autorest.NewBearerAuthorizer(spt)
	vaultURL := fmt.Sprintf("https://%s.vault.azure.net", vaultName)

	secretList, err := vaultClient.GetSecrets(context.TODO(),vaultURL, nil)
	if err != nil || secretList.Response == nil {
		return nil, fmt.Errorf("error on getting secrets list: %v", err)
	}

	for secretList.Values() != nil {

	    for _, secret := range secretList.Values() {
	    	secretKeyURL := strings.Split(*secret.ID, "/")
	    	secretKey := secretKeyURL[len(secretKeyURL)-1]
    
	    	if strings.HasPrefix(secretKey, servicePrefix) {
	    		secretName = secretKey[len(servicePrefix)+1:]
    
	    		if strings.HasSuffix(secretName, "-file") {
	    			secretName = secretName[:len(secretName)-5]
	    			secretFile = true
	    		} else {
	    			secretFile = false
	    		}
    
	    		secret, err := vaultClient.GetSecret(context.TODO(),vaultURL, secretKey, "")
	    		if err != nil {
	    			return nil, fmt.Errorf("failed to obtain secret with key \"%s\" from vault: %v", secretKey, err)
	    		}
    
	    		secretValue := *secret.Value
	    		if secretFile {
	    			secretDest = strings.Split(string(secretValue), " ")[0]
	    			secretValue = strings.Split(string(secretValue), " ")[1]
	    			secretContent, err := base64.StdEncoding.DecodeString(secretValue)
	    			if err != nil {
	    				return nil, fmt.Errorf("failed to base64 decode secret with key \"%s\": %v", secretKey, err)
	    			}
    
	    			err = ioutil.WriteFile(secretDest, secretContent, 0644)
	    			if err != nil {
	    				return nil, fmt.Errorf("failed to write contents of \"%s\" secret to \"%s\": %v", secretName, secretDest, exportStrings)
	    			}
	    			secretValue = secretDest
	    		}
    
	    		varName := strings.Replace(strings.ToUpper(secretName), "-", "_", -1)
	    		exportVars[varName] = secretValue
	    		if err != nil {
	    			return nil, fmt.Errorf("failed to update environment variable \"%s\": %v", secretName, err)
	    		}
	    	}
    
	    }
	    
	    secretList.NextWithContext(context.TODO())
	}

	return exportVars, nil
}
