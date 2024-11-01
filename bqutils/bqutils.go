package bqutils

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/bigquery"
	kms "cloud.google.com/go/kms/apiv1"
	"google.golang.org/api/iterator"

	"github.com/Vodafone/vault-plugin-aead/aeadutils"
	"github.com/google/tink/go/insecurecleartextkeyset"
	"github.com/google/tink/go/keyset"
	hclog "github.com/hashicorp/go-hclog"
	cmap "github.com/orcaman/concurrent-map"

	// kmspb "google.golang.org/genproto/googleapis/cloud/kms/v1"
	kmspb "cloud.google.com/go/kms/apiv1/kmspb"
)

type Options struct {
	projectId           string
	encryptDatasetId    string
	decryptDatasetId    string
	encryptRoutineId    string
	decryptRoutineId    string
	detRoutinePrefix    string
	nondetRoutinePrefix string
	kmsKeyName          string
	fieldName           string
}

func GetBQDatasets(ctx context.Context, projectId string) (map[string]*bigquery.Dataset, error) {

	bigqueryClient, err := bigquery.NewClient(ctx, projectId)
	if err != nil {
		hclog.L().Error("failed to setup bigqueryclient:  %v", err)
		return nil, err
	}
	defer bigqueryClient.Close()

	// Fetch the datasets in the project
	it := bigqueryClient.Datasets(ctx)

	datasets := map[string]*bigquery.Dataset{}
	for {
		ds, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			hclog.L().Error("Failed to iterate through datasets: ", err)
		}

		datasets[ds.DatasetID] = ds
	}
	return datasets, nil
}

func DoBQSync(ctx context.Context, kh *keyset.Handle, fieldName string, deterministic bool, envOptions cmap.ConcurrentMap, datasets map[string]*bigquery.Dataset) {

	// fieldName might have a "-" in it, but "-" are not allowed in BQ, so translate them to "_"
	fieldName = strings.Replace(fieldName, "-", "_", -1)
	fieldName = aeadutils.RemoveKeyPrefix(fieldName)

	var options Options
	resolveOptions(&options, fieldName, deterministic, envOptions)

	// 0. Initate clients
	kmsClient, err := kms.NewKeyManagementClient(ctx)

	if err != nil {
		hclog.L().Error("failed to setup client:  %v", err)
	}
	defer kmsClient.Close()

	binaryKeyset := new(bytes.Buffer)
	insecurecleartextkeyset.Write(kh, keyset.NewBinaryWriter(binaryKeyset))

	// loop through possible permeatations
	regionlist := [5]string{"unspecified", "eu", "europe_west1", "europe_west2", "europe_west3"} // note that these map to expected dataset names so EU is lower case and europe-west1 has underscore instead of dash

	var wg sync.WaitGroup

	for _, region := range regionlist {

		newOptions := Options(options)
		// options.encryptDatasetId = "vf<lm>_dh_lake_aead_encrypt_<region>_lv_s"
		// options.decryptDatasetId = "vf<lm>_dh_lake_<category>_aead_decrypt_<region>_lv_s"

		// first a simple substitution for <category>
		if strings.Contains(options.encryptDatasetId, "_<category>") {
			newOptions.encryptDatasetId = strings.Replace(options.encryptDatasetId, "<category>", fieldName, -1)
		}
		if strings.Contains(options.decryptDatasetId, "_<category>") {
			newOptions.decryptDatasetId = strings.Replace(options.decryptDatasetId, "<category>", fieldName, -1)
		}

		if region == "unspecified" {
			newOptions.encryptDatasetId = strings.Replace(newOptions.encryptDatasetId, "_<region>", "", -1)
			newOptions.decryptDatasetId = strings.Replace(newOptions.decryptDatasetId, "_<region>", "", -1)
		} else {
			newOptions.encryptDatasetId = strings.Replace(newOptions.encryptDatasetId, "<region>", region, -1)
			newOptions.decryptDatasetId = strings.Replace(newOptions.decryptDatasetId, "<region>", region, -1)
		}

		encryptDataset, encryptDatasetExists := datasets[newOptions.encryptDatasetId]
		decryptDataset, decryptDatasetExists := datasets[newOptions.decryptDatasetId]

		if encryptDatasetExists {
			// search for the ENCRYPT dataset
			md, err := encryptDataset.Metadata(ctx)
			if err == nil {
				actualDatasetRegion := strings.ToLower(md.Location)

				// infer the kms name
				// kms has the form:
				// projects/<project>/locations/<region>/keyRings/hsm-key-tink-<lm>-<region>/cryptoKeys/bq-key
				// and needs to be translated into
				// projects/<project>/locations/europe/keyRings/hsm-key-tink-<lm>-europe/cryptoKeys/bq-key
				// or
				// projects/<project>/locations/europe-west1/keyRings/hsm-key-tink-<lm>-europe-west1/cryptoKeys/bq-key

				expectedKMSRegion := actualDatasetRegion
				if actualDatasetRegion == "eu" {
					expectedKMSRegion = "europe"
				}
				newOptions.kmsKeyName = strings.Replace(newOptions.kmsKeyName, "<region>", expectedKMSRegion, -1)

				// // does the kms exist
				req := &kmspb.GetCryptoKeyRequest{
					Name: newOptions.kmsKeyName,
				}
				_, err := kmsClient.GetCryptoKey(ctx, req)

				if err == nil {
					// now we have a valid dataset and a valid kms (this doesn't mean we have access though)
					// 2. Wrap the binary keyset with KMS.

					encryptReq := &kmspb.EncryptRequest{
						Name:      newOptions.kmsKeyName,
						Plaintext: binaryKeyset.Bytes(),
					}

					encryptResp, err := kmsClient.Encrypt(ctx, encryptReq)
					if err != nil {
						hclog.L().Error("Failed to encrypt keyset:  %v", err)
					}

					// 3. Format the wrapped keyset as an escaped bytestring (like '\x00\x01\xAD') so BQ can accept it.
					escapedWrappedKeyset := ""
					for _, cbyte := range encryptResp.Ciphertext {
						escapedWrappedKeyset += fmt.Sprintf("\\x%02x", cbyte)
					}

					wg.Add(1)
					go func() {
						defer wg.Done()
						doBQRoutineCreateOrUpdate(ctx, newOptions, escapedWrappedKeyset, deterministic, "encrypt", encryptDataset)
					}()
				} else {
					hclog.L().Info("Failed to find kms key: " + newOptions.kmsKeyName)
				}
			} else {
				hclog.L().Info("Failed to find dataset: " + newOptions.encryptDatasetId)
			}

		}
		if decryptDatasetExists {
			// search for the DECRYPT dataset
			md, err := decryptDataset.Metadata(ctx)
			if err == nil {
				actualDatasetRegion := strings.ToLower(md.Location)

				// infer the kms name
				// kms has the form:
				// projects/<project>/locations/<region>/keyRings/hsm-key-tink-<lm>-<region>/cryptoKeys/bq-key
				// and needs to be translated into
				// projects/<project>/locations/europe/keyRings/hsm-key-tink-<lm>-europe/cryptoKeys/bq-key
				// or
				// projects/<project>/locations/europe-west1/keyRings/hsm-key-tink-<lm>-europe-west1/cryptoKeys/bq-key

				expectedKMSRegion := actualDatasetRegion
				if actualDatasetRegion == "eu" {
					expectedKMSRegion = "europe"
				}
				newOptions.kmsKeyName = strings.Replace(newOptions.kmsKeyName, "<region>", expectedKMSRegion, -1)

				// // does the kms exist
				req := &kmspb.GetCryptoKeyRequest{
					Name: newOptions.kmsKeyName,
				}
				_, err := kmsClient.GetCryptoKey(ctx, req)

				if err == nil {
					// now we have a valid dataset and a valid kms (this doesn't mean we have access though)
					// 2. Wrap the binary keyset with KMS.

					encryptReq := &kmspb.EncryptRequest{
						Name:      newOptions.kmsKeyName,
						Plaintext: binaryKeyset.Bytes(),
					}

					encryptResp, err := kmsClient.Encrypt(ctx, encryptReq)
					if err != nil {
						hclog.L().Error("Failed to encrypt keyset:  %v", err)
					}

					// 3. Format the wrapped keyset as an escaped bytestring (like '\x00\x01\xAD') so BQ can accept it.
					escapedWrappedKeyset := ""
					for _, cbyte := range encryptResp.Ciphertext {
						escapedWrappedKeyset += fmt.Sprintf("\\x%02x", cbyte)
					}
					wg.Add(1)
					go func() {
						defer wg.Done()
						doBQRoutineCreateOrUpdate(ctx, newOptions, escapedWrappedKeyset, deterministic, "decrypt", decryptDataset)
					}()
				} else {
					hclog.L().Info("Failed to find kms key: " + newOptions.kmsKeyName)
				}
			} else {
				hclog.L().Info("Failed to find dataset: " + newOptions.decryptDatasetId)
			}

		}

	}
	wg.Wait()
}

func doBQRoutineCreateOrUpdate(ctx context.Context, options Options, escapedWrappedKeyset string, deterministic bool, routineType string, dataset *bigquery.Dataset) {

	var err error

	if routineType == "encrypt" {
		// 4. Create a BigQuery Routine. You'll likely want to create one Routine each for encryption/decryption.
		routineEncryptBody := fmt.Sprintf("AEAD.ENCRYPT(KEYS.KEYSET_CHAIN(\"gcp-kms://%s\", b\"%s\"), plaintext, aad)", options.kmsKeyName, escapedWrappedKeyset)
		if deterministic {
			routineEncryptBody = fmt.Sprintf("DETERMINISTIC_ENCRYPT(KEYS.KEYSET_CHAIN(\"gcp-kms://%s\", b\"%s\"), plaintext, aad)", options.kmsKeyName, escapedWrappedKeyset)
		}

		routineEncryptRef := dataset.Routine(options.encryptRoutineId)
		routineExists := true
		var rm *bigquery.RoutineMetadata
		rm, err = routineEncryptRef.Metadata(ctx)
		if err != nil {
			// try again - api's seem a bit flakey
			time.Sleep(1 * time.Second)
			routineEncryptRef := dataset.Routine(options.encryptRoutineId)
			rm, err = routineEncryptRef.Metadata(ctx)
			if err != nil {
				routineExists = false
			}
		}

		if !routineExists {
			metadataEncrypt := &bigquery.RoutineMetadata{
				Type:     "SCALAR_FUNCTION",
				Language: "SQL",
				Body:     routineEncryptBody,
				Arguments: []*bigquery.RoutineArgument{
					{Name: "plaintext", DataType: &bigquery.StandardSQLDataType{TypeKind: "STRING"}},
					{Name: "aad", DataType: &bigquery.StandardSQLDataType{TypeKind: "STRING"}},
				},
			}
			err := routineEncryptRef.Create(ctx, metadataEncrypt)
			if err != nil {
				hclog.L().Error("Failed to create encrypt routine: " + options.encryptDatasetId + ":" + options.encryptRoutineId + " Error:" + err.Error())
			} else {
				hclog.L().Info("Encrypt Routine successfully created! " + options.encryptDatasetId + ":" + options.encryptRoutineId)
			}
		} else {
			metadataUpdatetoUpdate := &bigquery.RoutineMetadataToUpdate{
				Type:     "SCALAR_FUNCTION",
				Language: "SQL",
				Body:     routineEncryptBody,
				Arguments: []*bigquery.RoutineArgument{
					{Name: "plaintext", DataType: &bigquery.StandardSQLDataType{TypeKind: "STRING"}},
					{Name: "aad", DataType: &bigquery.StandardSQLDataType{TypeKind: "STRING"}},
				},
			}
			_, err = routineEncryptRef.Update(ctx, metadataUpdatetoUpdate, rm.ETag)
			if err != nil {
				hclog.L().Error("Failed to update encrypt routine: " + options.encryptDatasetId + ":" + options.encryptRoutineId + " Error:" + err.Error())
			} else {
				hclog.L().Info("Encrypt Routine successfully updated! " + options.encryptDatasetId + ":" + options.encryptRoutineId)
			}
		}
	} else {
		// we are doing a decrypt routine
		routineDecryptBody := fmt.Sprintf("AEAD.DECRYPT_STRING(KEYS.KEYSET_CHAIN(\"gcp-kms://%s\", b\"%s\"), ciphertext, aad)", options.kmsKeyName, escapedWrappedKeyset)
		if deterministic {
			//routineDecryptBody = fmt.Sprintf("DETERMINISTIC_DECRYPT_BYTES(KEYS.KEYSET_CHAIN(\"gcp-kms://%s\", b\"%s\"), ciphertext, aad)", options.kmsKeyName, escapedWrappedKeyset)
			routineDecryptBody = fmt.Sprintf("DETERMINISTIC_DECRYPT_STRING(KEYS.KEYSET_CHAIN(\"gcp-kms://%s\", b\"%s\"), ciphertext, aad)", options.kmsKeyName, escapedWrappedKeyset)
		}

		routineDecryptRef := dataset.Routine(options.decryptRoutineId)

		routineExists := true
		rm, err := routineDecryptRef.Metadata(ctx)
		if err != nil {
			routineExists = false
		}
		//	fmt.Printf("routineExists=%v", routineExists)

		if !routineExists {
			// routine DOES exist
			var metadataDecrypt *bigquery.RoutineMetadata
			if deterministic {
				// deterministic
				metadataDecrypt = &bigquery.RoutineMetadata{
					Type:     "SCALAR_FUNCTION",
					Language: "SQL",
					Body:     routineDecryptBody,
					Arguments: []*bigquery.RoutineArgument{
						{Name: "ciphertext", DataType: &bigquery.StandardSQLDataType{TypeKind: "BYTES"}},
						{Name: "aad", DataType: &bigquery.StandardSQLDataType{TypeKind: "STRING"}},
					},
				}
			} else {
				// non deterministic
				metadataDecrypt = &bigquery.RoutineMetadata{
					Type:     "SCALAR_FUNCTION",
					Language: "SQL",
					Body:     routineDecryptBody,
					Arguments: []*bigquery.RoutineArgument{
						{Name: "ciphertext", DataType: &bigquery.StandardSQLDataType{TypeKind: "BYTES"}},
						{Name: "aad", DataType: &bigquery.StandardSQLDataType{TypeKind: "STRING"}},
					},
				}
			}
			err = routineDecryptRef.Create(ctx, metadataDecrypt)
			if err != nil {
				hclog.L().Error("Failed to create decrypt routine: " + options.decryptDatasetId + ":" + options.decryptRoutineId + " Error:" + err.Error())
			} else {
				hclog.L().Info("Decrypt Routine successfully created! " + options.decryptDatasetId + ":" + options.decryptRoutineId)
			}
		} else {
			// routine DOES NOT exist
			var metadataUpdatetoUpdate *bigquery.RoutineMetadataToUpdate
			if deterministic {
				metadataUpdatetoUpdate = &bigquery.RoutineMetadataToUpdate{
					Type:     "SCALAR_FUNCTION",
					Language: "SQL",
					Body:     routineDecryptBody,
					Arguments: []*bigquery.RoutineArgument{
						{Name: "ciphertext", DataType: &bigquery.StandardSQLDataType{TypeKind: "BYTES"}},
						{Name: "aad", DataType: &bigquery.StandardSQLDataType{TypeKind: "STRING"}},
					},
				}
			} else {
				metadataUpdatetoUpdate = &bigquery.RoutineMetadataToUpdate{
					Type:     "SCALAR_FUNCTION",
					Language: "SQL",
					Body:     routineDecryptBody,
					Arguments: []*bigquery.RoutineArgument{
						{Name: "ciphertext", DataType: &bigquery.StandardSQLDataType{TypeKind: "BYTES"}},
						{Name: "aad", DataType: &bigquery.StandardSQLDataType{TypeKind: "STRING"}},
					},
				}
			}
			_, err = routineDecryptRef.Update(ctx, metadataUpdatetoUpdate, rm.ETag)
			if err != nil {
				hclog.L().Error("Failed to update decrypt routine: " + options.decryptDatasetId + ":" + options.decryptRoutineId + " Error:" + err.Error())
			} else {
				hclog.L().Info("Decrypt Routine successfully updated! " + options.decryptDatasetId + ":" + options.decryptRoutineId)
			}
		}
	}
}

func resolveOptions(options *Options, fieldName string, deterministic bool, envOptions cmap.ConcurrentMap) {

	// set the defaults
	options.kmsKeyName = "projects/your-kms-project/locations/<region>/keyRings/hsm-key-tink-<lm>-<region>/cryptoKeys/bq-key" // Format: 'projects/.../locations/.../keyRings/.../cryptoKeys/...'
	options.projectId = "your-bq-project"
	options.encryptDatasetId = "vf<lm>_dh_lake_aead_encrypt_<region>_lv_s"
	options.decryptDatasetId = "vf<lm>_dh_lake_<category>_aead_decrypt_<region>_lv_s"
	options.detRoutinePrefix = "siv"
	options.nondetRoutinePrefix = "gcm"

	// set any overrides
	kmsKeyInterface, ok := envOptions.Get("BQ_KMSKEY")
	if ok {
		options.kmsKeyName = fmt.Sprintf("%s", kmsKeyInterface)
	}
	projectIdInterface, ok := envOptions.Get("BQ_PROJECT")
	if ok {
		options.projectId = fmt.Sprintf("%s", projectIdInterface)
	}
	encryptDatasetIdInterface, ok := envOptions.Get("BQ_DEFAULT_ENCRYPT_DATASET")
	if ok {
		options.encryptDatasetId = fmt.Sprintf("%s", encryptDatasetIdInterface)
	}
	decryptDatasetIdInterface, ok := envOptions.Get("BQ_DEFAULT_DECRYPT_DATASET")
	if ok {
		options.decryptDatasetId = fmt.Sprintf("%s", decryptDatasetIdInterface)
	}
	detRoutinePrefixInterface, ok := envOptions.Get("BQ_ROUTINE_DET_PREFIX")
	if ok {
		options.detRoutinePrefix = fmt.Sprintf("%s", detRoutinePrefixInterface)
	}
	nondetRoutinePrefixInterface, ok := envOptions.Get("BQ_ROUTINE_NONDET_PREFIX")
	if ok {
		options.nondetRoutinePrefix = fmt.Sprintf("%s", nondetRoutinePrefixInterface)
	}

	// fieldName might have a "-" in it, but "-" are not allowed in BQ, so translate them to "_"
	options.fieldName = strings.Replace(fieldName, "-", "_", -1)
	if deterministic {
		options.encryptRoutineId = options.fieldName + "_" + options.detRoutinePrefix + "_encrypt" // ie routine name = address_siv_encrypt
		options.decryptRoutineId = options.fieldName + "_" + options.detRoutinePrefix + "_decrypt" // ie routine name = address_siv_decrypt
	} else {
		options.encryptRoutineId = options.fieldName + "_" + options.nondetRoutinePrefix + "_encrypt" // ie routine name = address_gcm_encrypt
		options.decryptRoutineId = options.fieldName + "_" + options.nondetRoutinePrefix + "_decrypt" // ie routine name = address_gcm_encrypt
	}

	// if we have a config entry for the encrypt or decrypt routine then use that as the dataset
	overrideBQDatasetInterface, ok := envOptions.Get(options.encryptRoutineId)
	if ok {
		options.encryptDatasetId = fmt.Sprintf("%s", overrideBQDatasetInterface)
	}
	overrideBQDatasetInterface, ok = envOptions.Get(options.decryptRoutineId)
	if ok {
		options.decryptDatasetId = fmt.Sprintf("%s", overrideBQDatasetInterface)
	}
}
