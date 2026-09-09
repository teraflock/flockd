//go:build !linux

package hardware

// vulkanVendors: no Vulkan lane off Linux. macOS is Metal-only (MoltenVK is
// not a target); Windows detection (vulkan-1.dll + registry ICDs) belongs
// to the Windows epic, teraflock/docs#9.
func vulkanVendors() map[string]bool { return nil }
