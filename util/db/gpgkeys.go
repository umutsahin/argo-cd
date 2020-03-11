package db

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"

	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/semaphore"

	appsv1 "github.com/argoproj/argo-cd/pkg/apis/application/v1alpha1"
	"github.com/argoproj/argo-cd/util/gpg"
)

// We allow only only one process at a single time to modify GPG keys sync state
var syncSemaphore = semaphore.NewWeighted(1)

// Validates a single GnuPG key and returns the key's ID
func validatePGPKey(keyData string) (string, error) {
	f, err := ioutil.TempFile("", "gpg-public-key")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())

	err = ioutil.WriteFile(f.Name(), []byte(keyData), 0600)
	if err != nil {
		return "", err
	}
	f.Close()

	parsed, err := gpg.ValidatePGPKeys(f.Name())
	if err != nil {
		return "", err
	}

	// Each key/value pair in the config map must exactly contain one public key, with the (short) GPG key ID as key
	if len(parsed) != 1 {
		return "", fmt.Errorf("More than one key found in input data")
	}

	return parsed[0], nil
}

// Reads all configured GPG public keys from the ConfigMap and returns information about them
func (db *db) ListConfiguredGPGPublicKeys(ctx context.Context) (map[string]string, error) {
	log.Debugf("Loading PGP public keys from config map")
	result := make(map[string]string, 0)
	keysCM, err := db.settingsMgr.GetConfigMapByName("argocd-gpg-cm")
	if err != nil {
		return nil, err
	}

	// We have to verify all PGP keys in the ConfigMap to be valid keys before. To do so,
	// we write each single one out to a temporary file and validate them through gpg.
	// This is not optimal, but the executil from argo-pkg does not support writing to
	// stdin of the forked process. So for now, we must live with that.
	for k, p := range keysCM.Data {
		if expectedKeyID := gpg.KeyID(k); expectedKeyID != "" {
			parsedKeyID, err := validatePGPKey(p)
			if err != nil {
				return nil, fmt.Errorf("Could not parse GPG key for entry '%s': %s", expectedKeyID, err.Error())
			}
			if expectedKeyID != parsedKeyID {
				return nil, fmt.Errorf("Key parsed for entry with key ID '%s' had different key ID '%s'", expectedKeyID, parsedKeyID)
			}
			result[parsedKeyID] = p
		} else {
			return nil, fmt.Errorf("Found entry with key '%s' in ConfigMap, but this is not a valid PGP key ID", k)
		}
	}

	return result, nil
}

// List all GPG public keys actually installed in the keyring
func (db *db) ListInstalledGPGPublicKeys(ctx context.Context) (map[string]*appsv1.GnuPGPublicKey, error) {
	result := make(map[string]*appsv1.GnuPGPublicKey, 0)

	keys, err := gpg.GetInstalledPGPKeys(nil)
	if err != nil {
		return nil, err
	}

	for _, v := range keys {
		if isSecret, err := gpg.IsSecretKey(v.KeyID); err == nil && !isSecret {
			result[v.KeyID] = v
		} else if err != nil {
			return nil, err
		}
	}

	return result, nil
}

// SynchronizeGPGPublicKeys synchronizes the installed keys with the configured keys
func (db *db) SynchronizeGPGPublicKeys(ctx context.Context) error {

	if !syncSemaphore.TryAcquire(1) {
		return fmt.Errorf("GnuPG database is locked, try again.")
	}
	defer syncSemaphore.Release(1)

	configuredKeys, err := db.ListConfiguredGPGPublicKeys(ctx)
	if err != nil {
		return err
	}

	installedKeys, err := db.ListInstalledGPGPublicKeys(ctx)
	if err != nil {
		return err
	}

	// Import all keys that are configured but not yet in the keyring
	for keyID, keyData := range configuredKeys {
		if _, ok := installedKeys[keyID]; !ok {
			log.Infof("Importing key ID '%s' from configuration", keyID)
			importedKeys, err := gpg.ImportPGPKeysFromString(keyData)
			if err != nil {
				log.Warnf("Could not import GPG key %s: %s", keyID, err.Error())
			} else {
				if keyID != importedKeys[0].KeyID {
					log.Warnf("KeyIDs differ, should not happen")
				}
			}
		}
	}

	// Remove all keys that are in the keyring, but not in the configuration anymore
	// We have a transient private key in the keyring whose ID we do not know, so we have
	// to check each key whether it's a private key, and skip it's removal. It won't be
	// in the configuration.
	for keyID, _ := range installedKeys {
		if _, ok := configuredKeys[keyID]; !ok {
			if isSecret, err := gpg.IsSecretKey(keyID); err == nil && !isSecret {
				log.Infof("Removing key ID '%s' from GnuPG's keyring", keyID)
				err := gpg.DeletePGPKey(keyID)
				if err != nil {
					log.Warnf("Could not delete key with key ID '%s': %s", keyID, err.Error())
				}
			} else if err != nil {
				log.Warnf("Error figuring out private key status for key ID %s: %s", keyID, err.Error())
			}
		}
	}

	return nil
}

// InitializeGPGKeyRing initializes a GnuPG keyring and imports all public keys configured in the ConfigMap
func (db *db) InitializeGPGKeyRing(ctx context.Context) (map[string]*appsv1.GnuPGPublicKey, error) {
	importedKeys := make(map[string]*appsv1.GnuPGPublicKey, 0)

	err := gpg.InitializeGnuPG()
	if err != nil {
		return nil, err
	}

	configuredKeys, err := db.ListConfiguredGPGPublicKeys(ctx)
	if err != nil {
		return nil, err
	}

	for _, key := range configuredKeys {
		f, err := ioutil.TempFile("", "gpg-import-key")
		if err != nil {
			return nil, err
		}
		defer os.Remove(f.Name())
		defer f.Close()
		_, err = f.WriteString(key)
		if err != nil {
			return nil, err
		}

		keys, err := gpg.ImportPGPKeys(f.Name())
		if err != nil {
			return nil, err
		}

		// This should not happen, because we already ensured singularity.
		// Anyhow, there could be a race and someone else wrote to our temp file (very unlikely)
		if len(keys) != 1 {
			return nil, fmt.Errorf("Unexpected key data.")
		}

		log.Infof("Imported GnuPG key with keyID '%s' to local keyring", keys[0].KeyID)
		importedKeys[keys[0].KeyID] = keys[0]
	}

	return importedKeys, nil
}
