# Trusted tokenizer helper

The Go helper implements the private C9 API. It starts a catalog-pinned `llama-server` child for each admitted model and only calls `/apply-template` and `/tokenize`. The HTTP helper never invokes an inference endpoint. Child processes bind a randomized `127.0.0.1` port, use a random API key stored in a temporary file, disable prompt-cache reuse and logging, and discard stdout/stderr. The helper passes the child a filtered environment so gateway, wallet, and API credentials are not inherited.

## Required inputs

- A signed model catalog in `CORDOVA_MODEL_CATALOG_FILE`.
- The matching 32-byte Ed25519 public key in `CORDOVA_MODEL_CATALOG_PUBLIC_KEY` or `CORDOVA_MODEL_CATALOG_PUBLIC_KEY_FILE` (base64 or hex).
- The helper bearer key in `CORDOVA_TOKENIZER_TOKEN` or `CORDOVA_TOKENIZER_TOKEN_FILE` (at least 32 printable bytes).
- A local model directory in `CORDOVA_TOKENIZER_MODEL_DIR`. Each GGUF must be named `<model-id>-<artifact_sha256>.gguf`; the helper checks its size and full SHA-256 before loading it.
- A `llama-server` executable, either on `PATH` or set with `CORDOVA_LLAMA_SERVER`. Its `--version` output must include a commit prefix matching the signed catalog's 40-character llama.cpp commit.

The helper does not download models or accept unsigned manifests. Example development command:

```powershell
$env:CORDOVA_MODEL_CATALOG_FILE = 'C:\models\catalog.json'
$env:CORDOVA_MODEL_CATALOG_PUBLIC_KEY_FILE = 'C:\models\catalog-public-key.txt'
$env:CORDOVA_TOKENIZER_TOKEN_FILE = 'C:\secrets\tokenizer-token.txt'
$env:CORDOVA_TOKENIZER_MODEL_DIR = 'C:\models\cache'
$env:CORDOVA_LLAMA_SERVER = 'C:\tools\llama-server.exe'
go run ./cmd/tokenizer
```

The default helper address is `127.0.0.1:9001`. A non-loopback listener requires both `CORDOVA_TOKENIZER_TLS_CERT_FILE` and `CORDOVA_TOKENIZER_TLS_KEY_FILE`; keep that listener on a private service network and configure the gateway with the corresponding CA through `CORDOVA_TOKENIZER_CA_FILE`. The helper bearer token is separate from the child API keys.

## Tokenization behavior

For input, the helper formats the supported messages with the loaded model's chat template, then requests token IDs with `add_special` from the signed BOS policy and `parse_special=true`. For output, it tokenizes the complete accumulated text with `add_special=false` and `parse_special=false`. It does not count stream chunks independently. Requests, templates, and token IDs stay in memory; the helper has no request-body logger.

Readiness verifies the exact catalog snapshot, child process health, loaded artifact path, one-slot configuration, runner commit, and SHA-256 of the reported chat template. Artifact SHA-256 binds the child to the cataloged GGUF; the separate tokenizer hash remains a signed catalog identity and must be backed by the release parity gate.

## Qualification status

This checkout does not include a qualified GGUF, a selected llama.cpp release, or helper/node parity evidence. A deployment must not admit a model for billing until `make test-tokenizer-parity` compares actual helper and node token IDs and verifies whole-text output counts for that exact signed artifact and llama.cpp commit. The current M1 gateway remains test-credit-only even when configured with a trusted tokenizer.

The child API follows the official llama.cpp server's `/apply-template`, `/tokenize`, `/props`, and `--api-key-file` interface. Re-verify that interface against the catalog-pinned source revision before release: [llama.cpp server documentation](https://github.com/ggml-org/llama.cpp/blob/master/tools/server/README.md).
