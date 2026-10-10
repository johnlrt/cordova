# Offline model catalog signing

`server/cmd/catalog-signer` creates an Ed25519 key seed and signs a catalog template that conforms to the C3 model manifest. Keep the key offline or in a protected operator key store. The gateway receives only the public key.

Create a new key on the offline signing machine:

```powershell
cd server
go run ./cmd/catalog-signer --generate-key-file C:\secure\cordova-catalog-seed.hex
```

The command refuses to overwrite an existing key file. On Unix systems it requires private key file permissions of `0600` or stricter and creates the seed file with `0600`; on Windows, place it in a directory protected by operator-managed ACLs. Back up the key under the organization's offline key recovery procedure.

Prepare a JSON snapshot using the fields in `contracts/ws/v1.schema.json` and `contracts/fixtures/ws/node-catalog-snapshot.json`. Each manifest must contain its full model, artifact URL/hash/size, tokenizer and template hashes, exact llama.cpp commit, BOS policy, memory bounds, SPDX license, manifest version, and an empty `signature` field. Use only artifact metadata and licenses that have been verified for the actual GGUF and runtime.

Sign to a new output path:

```powershell
go run ./cmd/catalog-signer `
  --input C:\secure\catalog-template.json `
  --output C:\secure\catalog-signed.json `
  --private-key-file C:\secure\cordova-catalog-seed.hex
```

The signer strictly parses the template, rejects duplicate JSON keys, unknown/missing fields, duplicate model IDs and unsafe artifacts, signs each canonical manifest, then verifies the complete snapshot before writing it. It creates output files without overwriting existing paths and prints the public key in base64. Configure `CORDOVA_MODEL_CATALOG_FILE` and `CORDOVA_MODEL_CATALOG_PUBLIC_KEY` (or `_FILE`) on the gateway with the resulting snapshot and public key.

The signer does not download or qualify model weights. The checked-in shared manifest is a test vector, not a production model catalog. M0 still requires a real artifact, reproducible conversion evidence, tokenizer/template parity and a protected signing-key procedure before any model can be admitted for real requests.
