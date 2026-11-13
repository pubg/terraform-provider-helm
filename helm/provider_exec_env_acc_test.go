// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package helm

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	authenticationv1 "k8s.io/api/authentication/v1"
	v1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"
)

// The exec plugin returns a token only when both variables from exec.env reach it, so the release can be installed
// only if the provider passes exec.env to the plugin.
const execEnvPluginScript = `#!/bin/sh
if [ "$EXEC_ENV_CHECK" != "expected" ] || [ -z "$EXEC_ENV_TOKEN" ]; then
  echo "exec.env was not passed to the exec plugin" >&2
  exit 1
fi
printf '{"apiVersion":"client.authentication.k8s.io/v1beta1","kind":"ExecCredential","status":{"token":"%s"}}' "$EXEC_ENV_TOKEN"
`

func TestAccResourceRelease_execEnv(t *testing.T) {
	name := randName("exec-env")
	namespace := createRandomNamespace(t)
	defer deleteNamespace(t, namespace)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6ProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: testAccExecEnvConfig(t, namespace, name, "expected"),
				Check:  resource.TestCheckResourceAttr("helm_release.test", "status", "deployed"),
			},
		},
	})
}

func TestAccResourceRelease_execEnvMismatch(t *testing.T) {
	name := randName("exec-env-x")
	namespace := createRandomNamespace(t)
	defer deleteNamespace(t, namespace)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6ProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config:      testAccExecEnvConfig(t, namespace, name, "unexpected"),
				ExpectError: regexp.MustCompile(`(?s)getting credentials: exec: executable.*exec-plugin.sh.*failed with exit code 1`),
			},
		},
	})
}

func testAccExecEnvConfig(t *testing.T, namespace, name, check string) string {
	token := createExecEnvServiceAccountToken(t, namespace, name)

	plugin := filepath.Join(t.TempDir(), "exec-plugin.sh")
	if err := os.WriteFile(plugin, []byte(execEnvPluginScript), 0o700); err != nil {
		t.Fatalf("could not write exec plugin: %s", err)
	}

	// an empty kubeconfig keeps the provider from using the credentials of KUBE_CONFIG_PATH instead of exec
	emptyKubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(emptyKubeconfig, []byte("apiVersion: v1\nkind: Config\n"), 0o600); err != nil {
		t.Fatalf("could not write empty kubeconfig: %s", err)
	}

	rest, err := clientcmd.BuildConfigFromFlags("", os.Getenv("KUBE_CONFIG_PATH"))
	if err != nil {
		t.Fatalf("could not load KUBE_CONFIG_PATH: %s", err)
	}

	return fmt.Sprintf(`
provider "helm" {
  kubernetes = {
    config_path            = %q
    host                   = %q
    cluster_ca_certificate = base64decode(%q)
    exec = {
      api_version = "client.authentication.k8s.io/v1beta1"
      command     = %q
      env = {
        EXEC_ENV_CHECK = %q
        EXEC_ENV_TOKEN = %q
      }
    }
  }
}

resource "helm_release" "test" {
  name      = %q
  namespace = %q
  chart     = %q
}
`, emptyKubeconfig, rest.Host, base64.StdEncoding.EncodeToString(rest.CAData), plugin, check, token, name, namespace, deletionPropagationChart)
}

func createExecEnvServiceAccountToken(t *testing.T, namespace, name string) string {
	ctx := context.TODO()
	sa := &v1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
	if _, err := client.CoreV1().ServiceAccounts(namespace).Create(ctx, sa, metav1.CreateOptions{}); err != nil {
		t.Fatalf("could not create service account: %s", err)
	}

	binding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "admin"},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: name, Namespace: namespace}},
	}
	if _, err := client.RbacV1().RoleBindings(namespace).Create(ctx, binding, metav1.CreateOptions{}); err != nil {
		t.Fatalf("could not create role binding: %s", err)
	}

	expiration := int64(3600)
	req := &authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{ExpirationSeconds: &expiration}}
	res, err := client.CoreV1().ServiceAccounts(namespace).CreateToken(ctx, name, req, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("could not create service account token: %s", err)
	}
	return res.Status.Token
}
