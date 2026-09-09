//go:build linux

package hardware

import "sync"

var (
	vulkanOnce   sync.Once
	vulkanResult map[string]bool
)

// vulkanVendors probes the running system once: a handful of stats plus
// reading the ICD manifests. Nil when the loader is absent.
func vulkanVendors() map[string]bool {
	vulkanOnce.Do(func() { vulkanResult = newLinuxVulkanProbe("/").vendors() })
	return vulkanResult
}
