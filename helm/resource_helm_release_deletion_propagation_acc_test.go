// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package helm

import (
	"context"
	"fmt"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	v1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	deletionPropagationChart     = "./testdata/charts/deletion-propagation"
	deletionPropagationFinalizer = "terraform-provider-helm.test/hold"
	deletionPropagationHold      = 20 * time.Second
)

// finalizerRelease removes the test finalizer from the dependent ConfigMap after a delay and records when it did.
type finalizerRelease struct {
	mu         sync.Mutex
	releasedAt time.Time
}

func (f *finalizerRelease) start(t *testing.T, namespace, name string, after time.Duration) {
	go func() {
		time.Sleep(after)
		removeDeletionPropagationFinalizer(t, namespace, name)
		f.mu.Lock()
		f.releasedAt = time.Now()
		f.mu.Unlock()
	}()
}

func (f *finalizerRelease) released() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.releasedAt
}

func TestAccResourceRelease_deletionPropagationForeground(t *testing.T) {
	name := randName("dp-fg")
	namespace := createRandomNamespace(t)
	defer deleteNamespace(t, namespace)

	dependent := name + "-dependent"
	hold := &finalizerRelease{}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6ProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: testAccHelmReleaseConfigDeletionPropagation(namespace, name, `deletion_propagation = "foreground"`, 120),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("helm_release.test", "deletion_propagation", "foreground"),
					createDeletionPropagationDependent(namespace, name, dependent),
				),
			},
			{
				PreConfig: func() { hold.start(t, namespace, dependent, deletionPropagationHold) },
				Config:    testAccHelmReleaseConfigEmpty,
				Check: func(_ *terraform.State) error {
					releasedAt := hold.released()
					if releasedAt.IsZero() {
						return fmt.Errorf("destroy finished before the dependent finalizer was removed")
					}
					if err := checkConfigMapGone(namespace, name+"-owner"); err != nil {
						return err
					}
					return checkConfigMapGone(namespace, dependent)
				},
			},
		},
	})
}

func TestAccResourceRelease_deletionPropagationBackground(t *testing.T) {
	name := randName("dp-bg")
	namespace := createRandomNamespace(t)
	defer deleteNamespace(t, namespace)

	dependent := name + "-dependent"
	hold := &finalizerRelease{}
	defer removeDeletionPropagationFinalizer(t, namespace, dependent)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6ProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: testAccHelmReleaseConfigDeletionPropagation(namespace, name, "", 120),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr("helm_release.test", "deletion_propagation"),
					createDeletionPropagationDependent(namespace, name, dependent),
				),
			},
			{
				PreConfig: func() { hold.start(t, namespace, dependent, deletionPropagationHold) },
				Config:    testAccHelmReleaseConfigEmpty,
				Check: func(_ *terraform.State) error {
					if !hold.released().IsZero() {
						return fmt.Errorf("destroy waited for the dependent finalizer without foreground propagation")
					}
					// the garbage collector marks the dependent asynchronously after the owner is gone
					deadline := time.Now().Add(10 * time.Second)
					for {
						cm, err := client.CoreV1().ConfigMaps(namespace).Get(context.TODO(), dependent, metav1.GetOptions{})
						if err != nil {
							return fmt.Errorf("dependent ConfigMap should still exist right after destroy: %w", err)
						}
						if cm.DeletionTimestamp != nil {
							return nil
						}
						if time.Now().After(deadline) {
							return fmt.Errorf("dependent ConfigMap was not marked for deletion by the garbage collector")
						}
						time.Sleep(time.Second)
					}
				},
			},
		},
	})
}

func TestAccResourceRelease_deletionPropagationTimeout(t *testing.T) {
	name := randName("dp-to")
	namespace := createRandomNamespace(t)
	defer deleteNamespace(t, namespace)

	dependent := name + "-dependent"
	defer removeDeletionPropagationFinalizer(t, namespace, dependent)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6ProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: testAccHelmReleaseConfigDeletionPropagation(namespace, name, `deletion_propagation = "foreground"`, 10),
				Check:  createDeletionPropagationDependent(namespace, name, dependent),
			},
			{
				Config:      testAccHelmReleaseConfigEmpty,
				ExpectError: regexp.MustCompile(`(?s)Error uninstalling release.*context deadline exceeded`),
			},
			{
				PreConfig: func() { removeDeletionPropagationFinalizer(t, namespace, dependent) },
				Config:    testAccHelmReleaseConfigEmpty,
			},
		},
	})
}

func TestAccResourceRelease_deletionPropagationInvalid(t *testing.T) {
	name := randName("dp-inv")
	namespace := createRandomNamespace(t)
	defer deleteNamespace(t, namespace)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6ProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config:      testAccHelmReleaseConfigDeletionPropagation(namespace, name, `deletion_propagation = "invalid"`, 120),
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`(?s)deletion_propagation.*foreground.*background.*orphan`),
			},
		},
	})
}

const testAccHelmReleaseConfigEmpty = `# no resources`

func testAccHelmReleaseConfigDeletionPropagation(namespace, name, attribute string, timeout int) string {
	return fmt.Sprintf(`
resource "helm_release" "test" {
  name      = %q
  namespace = %q
  chart     = %q
  wait      = true
  timeout   = %d
  %s
}
`, name, namespace, deletionPropagationChart, timeout, attribute)
}

// createDeletionPropagationDependent creates a ConfigMap owned by the release ConfigMap with blockOwnerDeletion and a
// finalizer, so that foreground deletion of the owner has to wait for the finalizer to be removed.
func createDeletionPropagationDependent(namespace, release, dependent string) resource.TestCheckFunc {
	return func(_ *terraform.State) error {
		owner, err := client.CoreV1().ConfigMaps(namespace).Get(context.TODO(), release+"-owner", metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("could not get owner ConfigMap: %w", err)
		}
		blockOwnerDeletion := true
		cm := &v1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:       dependent,
				Namespace:  namespace,
				Finalizers: []string{deletionPropagationFinalizer},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion:         "v1",
					Kind:               "ConfigMap",
					Name:               owner.Name,
					UID:                owner.UID,
					BlockOwnerDeletion: &blockOwnerDeletion,
				}},
			},
			Data: map[string]string{"role": "dependent"},
		}
		if _, err := client.CoreV1().ConfigMaps(namespace).Create(context.TODO(), cm, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("could not create dependent ConfigMap: %w", err)
		}
		return nil
	}
}

func removeDeletionPropagationFinalizer(t *testing.T, namespace, name string) {
	patch := []byte(`{"metadata":{"finalizers":null}}`)
	_, err := client.CoreV1().ConfigMaps(namespace).Patch(context.TODO(), name, "application/merge-patch+json", patch, metav1.PatchOptions{})
	if err != nil && !k8serrors.IsNotFound(err) {
		t.Errorf("could not remove finalizer from ConfigMap %s/%s: %s", namespace, name, err)
	}
}

func checkConfigMapGone(namespace, name string) error {
	_, err := client.CoreV1().ConfigMaps(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	if err == nil {
		return fmt.Errorf("ConfigMap %s/%s still exists", namespace, name)
	}
	if !k8serrors.IsNotFound(err) {
		return fmt.Errorf("could not get ConfigMap %s/%s: %w", namespace, name, err)
	}
	return nil
}
