package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"

	"gopkg.in/yaml.v2"

	aeadutils "github.com/Vodafone/vault-plugin-aead/aeadutils"
	"github.com/Vodafone/vault-plugin-aead/bqutils"
	kvutils "github.com/Vodafone/vault-plugin-aead/kvutils"

	cmap "github.com/orcaman/concurrent-map"
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
)

func main() {

	var c conf

	c.getConf()

	var envMap = cmap.New()
	envMap.Set("BQ_KMSKEY", c.KmsKeyName)
	envMap.Set("BQ_PROJECT", c.ProjectId)
	envMap.Set("BQ_DEFAULT_ENCRYPT_DATASET", c.EncryptDatasetId)
	envMap.Set("BQ_DEFAULT_DECRYPT_DATASET", c.DecryptDatasetId)
	envMap.Set("BQ_ROUTINE_DET_PREFIX", c.DetRoutinePrefix)
	envMap.Set("BQ_ROUTINE_NONDET_PREFIX", c.NondetRoutinePrefix)

	readKV(c, envMap)

}

type conf struct {
	VaultUrl            string   `yaml:"vaultUrl"`
	ApproleId           string   `yaml:"approleId"`
	SecretId            string   `yaml:"secretId"`
	Engine              string   `yaml:"engine"`
	EngineVersion       string   `yaml:"engineVersion"`
	ProjectId           string   `yaml:"projectId"`
	EncryptDatasetId    string   `yaml:"encryptDatasetId"`
	DecryptDatasetId    string   `yaml:"decryptDatasetId"`
	DetRoutinePrefix    string   `yaml:"detRoutinePrefix"`
	NondetRoutinePrefix string   `yaml:"nondetRoutinePrefix"`
	KmsKeyName          string   `yaml:"kmsKeyName"`
	KvKeys              []string `yaml:"kvKeys"`
}

func (c *conf) getConf() *conf {

	yamlFile, err := os.ReadFile("./conf.yaml")
	if err != nil {
		log.Printf("yamlFile.Get err   #%v ", err)
	}
	err = yaml.Unmarshal(yamlFile, c)
	if err != nil {
		log.Fatalf("Unmarshal: %v", err)
	}

	return c
}

func readKV(vaultconf conf, bqconfig cmap.ConcurrentMap) {
	ctx := context.Background()

	// get a client
	client, err := kvutils.KvGetClient(vaultconf.VaultUrl, "", vaultconf.ApproleId, vaultconf.SecretId)

	if err != nil {
		fmt.Print("\nfailed to initialize Vault client1")
		return
	}

	var paths []string
	if len(vaultconf.KvKeys) > 0 {
		paths = vaultconf.KvKeys
	} else {
		// read the paths recursively
		paths, err = kvutils.KvGetSecretPaths(client, vaultconf.Engine, vaultconf.EngineVersion, "")
	}

	if err != nil || paths == nil {
		fmt.Print("failed to read paths")
	}

	datasets, err := bqutils.GetBQDatasets(ctx, vaultconf.ProjectId)
	if err != nil {
		fmt.Println("Failed to list Datasets")
		return
	}

	var wg sync.WaitGroup

	// iterate through the paths
	for _, path := range paths {
		keyFound := false
		kvsecret, err := kvutils.KvGetSecret(client, vaultconf.Engine, vaultconf.EngineVersion, path)
		if err != nil || kvsecret.Data == nil {
			fmt.Print("\nfailed to read the secrets in folder " + path)
			continue
		}

		if strings.HasPrefix(path, "gcm/") || strings.HasPrefix(path, "siv/") {
			keyFound = true
			jsonKey, ok := kvsecret.Data["data"]
			if !ok {
				fmt.Printf("\nfailed to read back the aead engine %s key %s", vaultconf.Engine, path)
			}
			if _, kh, err := aeadutils.IsSecretAnAEADKeyset(jsonKey, path); err != nil {
				fmt.Printf("\nfailed to read valid secret engine %s key %s", vaultconf.Engine, path)
			} else {
				fmt.Print("\npath: " + path + " is a valid aeadkey\n")
				newkeyname := aeadutils.RemoveKeyPrefix(path)
				deterministic := aeadutils.IsKeyHandleDeterministic(kh)

				wg.Add(1)
				go func() {
					defer wg.Done()
					bqutils.DoBQSync(ctx, kh, newkeyname, deterministic, bqconfig, datasets)
				}()
			}
		}

		if !keyFound {
			fmt.Print("\nfailed to read back any keys in KV secret " + path)
		}

	}
	wg.Wait()
	return
}
