package nelmclient

import (
	"encoding/base64"
	"fmt"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// BuildInlineKubeconfig synthesizes a COMPLETE, self-contained kubeconfig from
// inline connection fields (host/token/cluster CA) and returns it base64-encoded
// for nelm's KubeConfigBase64.
//
// This is what makes the inline host/token/cluster_ca_certificate path behave
// like the hashicorp/kubernetes and hashicorp/helm providers: a STANDALONE
// connection to exactly this cluster. nelm layers its KubeConnectionOptions as
// clientcmd overrides on top of a loaded kubeconfig, and its ApplyDefaults loads
// ~/.kube/config whenever KubeConfigBase64 is empty — so passing only override
// fields would merge the caller's ambient current-context, and a token override
// against a client-cert/basic-auth current-context makes clientcmd reject the
// config ("more than one authentication method found"). Returning a full
// kubeconfig via KubeConfigBase64 both suppresses that ambient load (ApplyDefaults
// skips it) and provides a single, unambiguous auth method (the bearer token).
//
// insecure and caPEM are mutually exclusive at the Kubernetes API level (a CA
// cannot be combined with insecure-skip-tls-verify); insecure wins here and the
// CA is omitted when it is set.
func BuildInlineKubeconfig(host, token, caPEM string, insecure bool, tlsServerName string) (string, error) {
	if host == "" {
		return "", fmt.Errorf("inline kube connection requires a host")
	}

	cluster := clientcmdapi.NewCluster()
	cluster.Server = host
	cluster.TLSServerName = tlsServerName

	if insecure {
		cluster.InsecureSkipTLSVerify = true
	} else if caPEM != "" {
		cluster.CertificateAuthorityData = []byte(caPEM)
	}

	authInfo := clientcmdapi.NewAuthInfo()
	authInfo.Token = token

	context := clientcmdapi.NewContext()
	context.Cluster = "nelm"
	context.AuthInfo = "nelm"

	cfg := clientcmdapi.NewConfig()
	cfg.Clusters["nelm"] = cluster
	cfg.AuthInfos["nelm"] = authInfo
	cfg.Contexts["nelm"] = context
	cfg.CurrentContext = "nelm"

	raw, err := clientcmd.Write(*cfg)
	if err != nil {
		return "", fmt.Errorf("serialize inline kubeconfig: %w", err)
	}

	return base64.StdEncoding.EncodeToString(raw), nil
}
