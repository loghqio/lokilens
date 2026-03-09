package license

import "github.com/keygen-sh/machineid"

// machineFingerprint returns a stable, platform-specific machine ID
// hashed with the product ID to prevent cross-product fingerprint reuse.
func machineFingerprint(productID string) (string, error) {
	return machineid.ProtectedID(productID)
}
