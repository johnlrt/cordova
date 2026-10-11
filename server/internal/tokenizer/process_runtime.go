package tokenizer

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultNativeStartupTimeout = 2 * time.Minute
	nativeVersionTimeout        = 10 * time.Second
	nativeRequestTimeout        = 2 * time.Second
	nativeHealthInterval        = 250 * time.Millisecond
	maxNativeRequestBytes       = 8 << 20
	maxNativeResponseBytes      = 1 << 20
)

// ProcessConfig selects an already-installed llama-server and a directory of
// locally cached, signed-catalog artifacts. The helper never downloads models.
type ProcessConfig struct {
	BinaryPath     string
	ModelDir       string
	TempDir        string
	StartupTimeout time.Duration
}

// ProcessRuntime supervises one private loopback llama-server child per model.
// It calls only the template and tokenizer endpoints; it never calls inference.
type ProcessRuntime struct {
	mu     sync.RWMutex
	closed bool
	models map[string]*modelRunner
}

type modelRunner struct {
	model    Model
	artifact string
	baseURL  string
	apiKey   string
	keyFile  string
	client   *http.Client
	command  *exec.Cmd
	done     chan struct{}
}

type nativeTemplateRequest struct {
	Messages []nativeMessage `json:"messages"`
}

type nativeMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type nativeTemplateResponse struct {
	Prompt *string `json:"prompt"`
}

type nativeTokenizeRequest struct {
	Content      string `json:"content"`
	AddSpecial   bool   `json:"add_special"`
	ParseSpecial bool   `json:"parse_special"`
	WithPieces   bool   `json:"with_pieces"`
}

type nativeTokenizeResponse struct {
	Tokens []uint32 `json:"tokens"`
}

type nativeProperties struct {
	TotalSlots   int     `json:"total_slots"`
	ModelPath    string  `json:"model_path"`
	ChatTemplate *string `json:"chat_template"`
}

// NewProcessRuntime verifies the native binary and every artifact before
// starting a child. A partial startup is always torn down before returning.
func NewProcessRuntime(ctx context.Context, config ProcessConfig, models []Model) (*ProcessRuntime, error) {
	if ctx == nil || len(models) == 0 || len(models) > 256 || strings.TrimSpace(config.ModelDir) == "" {
		return nil, errors.New("tokenizer runtime configuration is invalid")
	}
	if config.StartupTimeout == 0 {
		config.StartupTimeout = defaultNativeStartupTimeout
	}
	if config.StartupTimeout < time.Second || config.StartupTimeout > 10*time.Minute {
		return nil, errors.New("tokenizer runtime startup timeout is invalid")
	}
	modelDir, err := filepath.Abs(config.ModelDir)
	if err != nil {
		return nil, errors.New("tokenizer model directory is unavailable")
	}
	resolvedDir, err := filepath.EvalSymlinks(modelDir)
	if err != nil || !samePath(modelDir, resolvedDir) {
		return nil, errors.New("tokenizer model directory is unavailable")
	}
	info, err := os.Stat(resolvedDir)
	if err != nil || !info.IsDir() {
		return nil, errors.New("tokenizer model directory is unavailable")
	}

	binary := strings.TrimSpace(config.BinaryPath)
	if binary == "" {
		binary, err = exec.LookPath("llama-server")
	} else {
		binary, err = exec.LookPath(binary)
	}
	if err != nil {
		return nil, errors.New("catalog-pinned llama-server is unavailable")
	}
	binary, err = filepath.Abs(binary)
	if err != nil {
		return nil, errors.New("catalog-pinned llama-server is unavailable")
	}

	runtime := &ProcessRuntime{models: make(map[string]*modelRunner, len(models))}
	catalogVersion := ""
	verifiedCommits := make(map[string]struct{})
	for _, model := range models {
		if err := validateProcessModel(model); err != nil {
			_ = runtime.Close()
			return nil, errors.New("tokenizer catalog model is invalid")
		}
		if catalogVersion == "" {
			catalogVersion = model.CatalogVersion
		} else if model.CatalogVersion != catalogVersion {
			_ = runtime.Close()
			return nil, errors.New("tokenizer catalog versions do not match")
		}
		if _, exists := runtime.models[model.ID]; exists {
			_ = runtime.Close()
			return nil, errors.New("tokenizer model identity is duplicated")
		}
		if _, ok := verifiedCommits[model.LlamaCommit]; !ok {
			if err := verifyNativeVersion(ctx, binary, model.LlamaCommit); err != nil {
				_ = runtime.Close()
				return nil, err
			}
			verifiedCommits[model.LlamaCommit] = struct{}{}
		}
		artifact := filepath.Join(resolvedDir, model.ID+"-"+model.ArtifactSHA256+".gguf")
		if err := verifyNativeArtifact(ctx, artifact, model.ArtifactSHA256, model.ArtifactSizeBytes); err != nil {
			_ = runtime.Close()
			return nil, err
		}
		runner, err := startModelRunner(ctx, config, binary, resolvedDir, artifact, model)
		if err != nil {
			_ = runtime.Close()
			return nil, err
		}
		runtime.models[model.ID] = runner
	}
	return runtime, nil
}

func validateProcessModel(model Model) error {
	if !validModelID(model.ID) || !validUUID(model.CatalogVersion) || model.ContextTokens == 0 ||
		model.ContextTokens > maxContextTokens || !validHash(model.ArtifactSHA256) ||
		!validHash(model.TokenizerSHA256) || !validHash(model.TemplateSHA256) ||
		model.ArtifactSizeBytes == 0 || model.ArtifactSizeBytes > 1<<50 || !validCommit(model.LlamaCommit) {
		return errors.New("invalid model")
	}
	return nil
}

func verifyNativeVersion(parent context.Context, binary, expectedCommit string) error {
	ctx, cancel := context.WithTimeout(parent, nativeVersionTimeout)
	defer cancel()
	output, err := exec.CommandContext(ctx, binary, "--version").CombinedOutput()
	if err != nil {
		return errors.New("catalog-pinned llama-server version could not be verified")
	}
	fields := strings.Fields(string(output))
	for i := 0; i+1 < len(fields); i++ {
		if strings.EqualFold(strings.Trim(fields[i], ":"), "commit") && nativeCommitMatches(expectedCommit, fields[i+1]) {
			return nil
		}
	}
	return errors.New("installed llama-server does not match the signed catalog revision")
}

func nativeCommitMatches(expected, reported string) bool {
	if !validCommit(expected) || len(reported) < 9 || len(reported) > 40 {
		return false
	}
	for _, char := range reported {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return strings.HasPrefix(expected, reported)
}

func verifyNativeArtifact(ctx context.Context, path, expectedHash string, expectedSize uint64) error {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 ||
		uint64(info.Size()) != expectedSize {
		return errors.New("signed model artifact is unavailable or has the wrong size")
	}
	file, err := os.Open(path)
	if err != nil {
		return errors.New("signed model artifact is unavailable")
	}
	defer file.Close()
	hasher := sha256.New()
	buffer := make([]byte, 256<<10)
	for {
		if err := ctx.Err(); err != nil {
			return errors.New("tokenizer startup was cancelled")
		}
		count, readErr := file.Read(buffer)
		if count > 0 {
			_, _ = hasher.Write(buffer[:count])
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return errors.New("signed model artifact could not be verified")
		}
	}
	if hex.EncodeToString(hasher.Sum(nil)) != expectedHash {
		return errors.New("signed model artifact hash does not match the catalog")
	}
	return nil
}

func startModelRunner(ctx context.Context, config ProcessConfig, binary, modelDir, artifact string, model Model) (*modelRunner, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, errors.New("tokenizer could not reserve a loopback port")
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()

	keyBytes := make([]byte, 32)
	if _, err := rand.Read(keyBytes); err != nil {
		return nil, errors.New("tokenizer could not create a child credential")
	}
	apiKey := base64.RawURLEncoding.EncodeToString(keyBytes)
	keyFile, err := writeNativeKeyFile(config.TempDir, apiKey)
	if err != nil {
		return nil, errors.New("tokenizer could not create a child credential")
	}

	args := []string{
		"--model", artifact,
		"--alias", model.ID,
		"--host", "127.0.0.1",
		"--port", strconv.Itoa(port),
		"--parallel", "1",
		"--ctx-size", strconv.FormatUint(uint64(model.ContextTokens), 10),
		"--api-key-file", keyFile,
		"--no-cache-prompt",
		"--no-cache-idle-slots",
		"--no-webui",
		"--log-disable",
	}
	command := exec.Command(binary, args...)
	command.Dir = modelDir
	command.Stdin = nil
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	command.Env = nativeEnvironment(os.Environ())
	if err := command.Start(); err != nil {
		_ = os.Remove(keyFile)
		return nil, errors.New("catalog-pinned llama-server could not be started")
	}

	transport := &http.Transport{
		Proxy:               nil,
		DialContext:         (&net.Dialer{Timeout: nativeRequestTimeout, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConnsPerHost: 2,
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   nativeRequestTimeout + time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	baseURL := "http://127.0.0.1:" + strconv.Itoa(port)
	runner := &modelRunner{
		model: model, artifact: artifact, baseURL: baseURL, apiKey: apiKey,
		keyFile: keyFile, client: client, command: command, done: make(chan struct{}),
	}
	go func() {
		_ = command.Wait()
		close(runner.done)
	}()

	readyContext, cancel := context.WithTimeout(ctx, config.StartupTimeout)
	defer cancel()
	for {
		select {
		case <-runner.done:
			runner.stop()
			return nil, errors.New("catalog-pinned llama-server exited during startup")
		default:
		}
		healthContext, healthCancel := context.WithTimeout(readyContext, nativeRequestTimeout)
		healthErr := runner.get(healthContext, "/health", nil)
		healthCancel()
		if healthErr == nil {
			if err := runner.verifyProperties(readyContext); err != nil {
				runner.stop()
				return nil, err
			}
			return runner, nil
		}
		select {
		case <-readyContext.Done():
			runner.stop()
			return nil, errors.New("catalog-pinned llama-server did not become ready")
		case <-runner.done:
			runner.stop()
			return nil, errors.New("catalog-pinned llama-server exited during startup")
		case <-time.After(nativeHealthInterval):
		}
	}
}

func nativeEnvironment(environment []string) []string {
	allowed := map[string]struct{}{
		"PATH": {}, "SYSTEMROOT": {}, "WINDIR": {}, "TEMP": {}, "TMP": {}, "TMPDIR": {},
		"LANG": {}, "LC_ALL": {}, "OMP_NUM_THREADS": {}, "LD_LIBRARY_PATH": {},
		"DYLD_LIBRARY_PATH": {}, "DYLD_FALLBACK_LIBRARY_PATH": {}, "CUDA_VISIBLE_DEVICES": {},
		"CUDA_PATH": {}, "CUDA_HOME": {}, "HIP_VISIBLE_DEVICES": {}, "ROCR_VISIBLE_DEVICES": {},
		"ROCM_PATH": {}, "GGML_VULKAN_DEVICE": {}, "VK_ICD_FILENAMES": {},
	}
	filtered := make([]string, 0, len(allowed))
	for _, item := range environment {
		name, _, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		if _, ok := allowed[strings.ToUpper(name)]; ok {
			filtered = append(filtered, item)
		}
	}
	return filtered
}

func writeNativeKeyFile(directory, key string) (string, error) {
	file, err := os.CreateTemp(directory, "cordova-tokenizer-key-*")
	if err != nil {
		return "", err
	}
	path := file.Name()
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return "", err
	}
	if _, err := io.WriteString(file, key+"\n"); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return "", err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return "", err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}
