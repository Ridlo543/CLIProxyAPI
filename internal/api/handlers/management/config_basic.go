package management

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
)

const (
	latestReleaseURL       = "https://api.github.com/repos/router-for-me/CLIProxyAPI/releases/latest"
	latestReleaseUserAgent = "CLIProxyAPI"
)

// configETag derives a strong validator from the exact bytes on disk, so two
// readers of the same file always compute the same tag.
func configETag(data []byte) string {
	sum := sha256.Sum256(data)
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

func (h *Handler) GetConfig(c *gin.Context) {
	if h == nil || h.cfg == nil {
		c.JSON(200, gin.H{})
		return
	}
	c.JSON(200, h.cfg)
}

type releaseInfo struct {
	TagName string `json:"tag_name"`
	Name    string `json:"name"`
}

func setLatestReleaseRequestHeaders(req *http.Request) {
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", latestReleaseUserAgent)
	if token := util.ResolveGitHubToken(); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

// GetLatestVersion returns the latest release version from GitHub without downloading assets.
func (h *Handler) GetLatestVersion(c *gin.Context) {
	client := &http.Client{Timeout: 10 * time.Second}
	proxyURL := ""
	if h != nil && h.cfg != nil {
		proxyURL = strings.TrimSpace(h.cfg.ProxyURL)
	}
	if proxyURL != "" {
		sdkCfg := &sdkconfig.SDKConfig{ProxyURL: proxyURL}
		util.SetProxy(sdkCfg, client)
	}

	req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodGet, latestReleaseURL, nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "request_create_failed", "message": err.Error()})
		return
	}
	setLatestReleaseRequestHeaders(req)

	resp, err := client.Do(req)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "request_failed", "message": err.Error()})
		return
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.WithError(errClose).Debug("failed to close latest version response body")
		}
	}()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		c.JSON(http.StatusBadGateway, gin.H{"error": "unexpected_status", "message": fmt.Sprintf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))})
		return
	}

	var info releaseInfo
	if errDecode := json.NewDecoder(resp.Body).Decode(&info); errDecode != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "decode_failed", "message": errDecode.Error()})
		return
	}

	version := strings.TrimSpace(info.TagName)
	if version == "" {
		version = strings.TrimSpace(info.Name)
	}
	if version == "" {
		c.JSON(http.StatusBadGateway, gin.H{"error": "invalid_response", "message": "missing release version"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"latest-version": version})
}

func WriteConfig(path string, data []byte) error {
	prepared, err := prepareConfigBytes(data)
	if err != nil {
		return err
	}
	return writeConfigBytes(path, prepared)
}

// prepareConfigBytes returns the exact bytes WriteConfig stores for data.
func prepareConfigBytes(data []byte) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) > 0 && config.IsV8ConfigLayout(doc.Content[0]) {
		var err error
		data, _, err = config.NormalizeConfigLayout(data, true)
		if err != nil {
			return nil, err
		}
	}
	return config.NormalizeCommentIndentation(data), nil
}

func writeConfigBytes(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	if _, errWrite := f.Write(data); errWrite != nil {
		_ = f.Close()
		return errWrite
	}
	if errSync := f.Sync(); errSync != nil {
		_ = f.Close()
		return errSync
	}
	return f.Close()
}

func (h *Handler) PutConfigYAML(c *gin.Context) {
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_yaml", "message": "cannot read request body"})
		return
	}
	var cfg config.Config
	if err = yaml.Unmarshal(body, &cfg); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_yaml", "message": err.Error()})
		return
	}
	// Validate config using LoadConfigOptional with optional=false to enforce parsing
	tmpDir := filepath.Dir(h.configFilePath)
	tmpFile, err := os.CreateTemp(tmpDir, "config-validate-*.yaml")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "write_failed", "message": err.Error()})
		return
	}
	tempFile := tmpFile.Name()
	if _, errWrite := tmpFile.Write(body); errWrite != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tempFile)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "write_failed", "message": errWrite.Error()})
		return
	}
	if errClose := tmpFile.Close(); errClose != nil {
		_ = os.Remove(tempFile)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "write_failed", "message": errClose.Error()})
		return
	}
	defer func() {
		_ = os.Remove(tempFile)
	}()
	_, err = config.LoadConfigOptional(tempFile, false)
	if err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_config", "message": err.Error()})
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	// Re-read under the lock: the tag has to describe the bytes this write is
	// actually replacing, not what the request handler saw earlier.
	current, errCurrent := os.ReadFile(h.configFilePath)
	if errCurrent != nil && !os.IsNotExist(errCurrent) {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "read_failed", "message": errCurrent.Error()})
		return
	}
	currentETag := configETag(current)
	if !ifMatchSatisfied(c.Request.Header.Values("If-Match"), currentETag) {
		// Hand back the live tag so the client can merge and retry without
		// a separate GET.
		c.Header("ETag", currentETag)
		c.JSON(http.StatusPreconditionFailed, gin.H{
			"error":   "config_changed",
			"message": "config.yaml changed since it was loaded",
		})
		return
	}
	if WriteConfig(h.configFilePath, body) != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "write_failed", "message": "failed to write config"})
		return
	}
	// Reload into handler to keep memory in sync
	newCfg, err := config.LoadConfig(h.configFilePath)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "reload_failed", "message": err.Error()})
		return
	}
	h.cfg = newCfg
	// Echo the tag of what was just stored so a client can chain saves.
	stored, errStored := os.ReadFile(h.configFilePath)
	if errStored == nil {
		c.Header("ETag", configETag(stored))
	} else {
		c.Header("ETag", configETag(body))
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "changed": []string{"config"}})
}

// configPathOperation is one targeted config mutation. Expect, when present,
// is the value the client read at Path; the mutation is refused unless the
// server reads the same value, so a client-side YAML parser that disagrees
// with this one cannot write back a misread subtree.
type configPathOperation struct {
	Path    []string        `json:"path"`
	Value   json.RawMessage `json:"value"`
	Delete  bool            `json:"delete"`
	Replace bool            `json:"replace"`
	Expect  json.RawMessage `json:"expect"`
}

// PatchConfigYAMLPath applies one validated mutation to a YAML mapping path while
// preserving every unrelated YAML node, including fields unknown to this build.
func (h *Handler) PatchConfigYAMLPath(c *gin.Context) {
	var request struct {
		Operations []configPathOperation `json:"operations"`
		configPathOperation
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_patch"})
		return
	}
	if len(request.Operations) == 0 && len(request.Path) > 0 {
		request.Operations = append(request.Operations, request.configPathOperation)
	}
	if len(request.Operations) == 0 || len(request.Operations) > 128 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_patch"})
		return
	}
	if len(c.Request.Header.Values("If-Match")) == 0 {
		c.JSON(http.StatusPreconditionRequired, gin.H{"error": "if_match_required"})
		return
	}
	values := make([]*yaml.Node, len(request.Operations))
	for index, operation := range request.Operations {
		if len(operation.Path) == 0 || len(operation.Value) == 0 && !operation.Delete {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_patch", "index": index})
			return
		}
		// Path segments are literal mapping keys, so a model name such as
		// "nvidia/llama" is a valid segment.
		for _, part := range operation.Path {
			if strings.TrimSpace(part) == "" || strings.ContainsRune(part, '\x00') {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_path", "index": index})
				return
			}
		}
		for previous := 0; previous < index; previous++ {
			if configPathsOverlap(request.Operations[previous].Path, operation.Path) {
				c.JSON(http.StatusBadRequest, gin.H{"error": "overlapping_paths", "index": index})
				return
			}
		}
		if !operation.Delete {
			var value yaml.Node
			if err := yaml.Unmarshal(operation.Value, &value); err != nil || len(value.Content) == 0 {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_patch_value", "index": index})
				return
			}
			blockStyleConfigYAMLValue(value.Content[0])
			values[index] = value.Content[0]
		}
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	current, errRead := os.ReadFile(h.configFilePath)
	if errRead != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "read_failed", "message": errRead.Error()})
		return
	}
	currentETag := configETag(current)
	if !ifMatchSatisfied(c.Request.Header.Values("If-Match"), currentETag) {
		c.Header("ETag", currentETag)
		c.JSON(http.StatusPreconditionFailed, gin.H{"error": "config_changed"})
		return
	}
	var document yaml.Node
	if err := yaml.Unmarshal(current, &document); err != nil || len(document.Content) == 0 || document.Content[0].Kind != yaml.MappingNode {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_config", "message": "config root must be a YAML mapping"})
		return
	}
	var currentDecoded any
	if err := yaml.Unmarshal(current, &currentDecoded); err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_config", "message": err.Error()})
		return
	}
	for index, operation := range request.Operations {
		if len(operation.Expect) == 0 {
			continue
		}
		var expected any
		if err := yaml.Unmarshal(operation.Expect, &expected); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_expect", "index": index})
			return
		}
		stored, found := configValueAt(currentDecoded, operation.Path)
		if !found || !configValuesEquivalent(expected, stored) {
			c.JSON(http.StatusConflict, gin.H{"error": "expect_mismatch", "index": index, "path": operation.Path})
			return
		}
	}
	merged := make([]bool, len(request.Operations))
	for index, operation := range request.Operations {
		parent := document.Content[0]
		for _, part := range operation.Path[:len(operation.Path)-1] {
			next := configYAMLMappingValue(parent, part)
			if next == nil {
				next = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
				parent.Content = append(parent.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: part}, next)
			}
			if next.Kind != yaml.MappingNode {
				c.JSON(http.StatusConflict, gin.H{"error": "path_not_mapping", "index": index})
				return
			}
			parent = next
		}
		key := operation.Path[len(operation.Path)-1]
		if operation.Delete {
			if !configYAMLDeleteMappingValue(parent, key) {
				c.JSON(http.StatusNotFound, gin.H{"error": "path_not_found", "index": index})
				return
			}
			continue
		}
		currentValue := configYAMLMappingValue(parent, key)
		updatedValue := values[index]
		if currentValue == nil {
			parent.Content = append(parent.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, updatedValue)
		} else if !operation.Replace && currentValue.Kind == yaml.MappingNode && updatedValue.Kind == yaml.MappingNode {
			mergeConfigYAMLMapping(currentValue, updatedValue)
			merged[index] = true
		} else {
			updatedValue.HeadComment, updatedValue.LineComment, updatedValue.FootComment = currentValue.HeadComment, currentValue.LineComment, currentValue.FootComment
			parent.Content[indexYAMLMappingValue(parent, key)+1] = updatedValue
		}
	}
	var output bytes.Buffer
	encoder := yaml.NewEncoder(&output)
	encoder.SetIndent(2)
	if err := encoder.Encode(&document); err != nil {
		_ = encoder.Close()
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_config", "message": err.Error()})
		return
	}
	if err := encoder.Close(); err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_config", "message": err.Error()})
		return
	}
	updatedBytes, err := prepareConfigBytes(output.Bytes())
	if err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_config", "message": err.Error()})
		return
	}
	validated, err := config.ParseConfigBytes(updatedBytes)
	if err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "invalid_config", "message": err.Error()})
		return
	}
	paths := make([][]string, len(request.Operations))
	deletes := make([]bool, len(request.Operations))
	for index, operation := range request.Operations {
		paths[index], deletes[index] = operation.Path, operation.Delete
	}
	if err := verifyConfigYAMLPatch(current, updatedBytes, paths, deletes, merged, values); err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "patch_not_preserved", "message": err.Error()})
		return
	}
	if err := writeConfigBytes(h.configFilePath, updatedBytes); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "write_failed", "message": err.Error()})
		return
	}
	h.cfg = validated
	snapshot := h.reloadSnapshotConfigLocked()
	if stored, err := os.ReadFile(h.configFilePath); err == nil {
		c.Header("ETag", configETag(stored))
	}
	h.reloadConfigAfterManagementSaveAsync(c.Request.Context(), snapshot)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// configPathsOverlap reports whether one path equals or contains the other.
func configPathsOverlap(a, b []string) bool {
	if len(a) > len(b) {
		a, b = b, a
	}
	for index := range a {
		if a[index] != b[index] {
			return false
		}
	}
	return true
}

// blockStyleConfigYAMLValue drops the JSON flow and double-quoted styles a
// patch value carries, so it is written like the rest of the document. The
// encoder still quotes any string that would otherwise read as another type.
func blockStyleConfigYAMLValue(node *yaml.Node) {
	switch node.Kind {
	case yaml.MappingNode:
		node.Style = 0
		if len(node.Content) == 0 {
			node.Style = yaml.FlowStyle
		}
	case yaml.SequenceNode:
		node.Style = 0
		if len(node.Content) == 0 {
			node.Style = yaml.FlowStyle
		}
	case yaml.ScalarNode:
		if node.Style == yaml.DoubleQuotedStyle {
			node.Style = 0
		}
	}
	for _, child := range node.Content {
		blockStyleConfigYAMLValue(child)
	}
}

// verifyConfigYAMLPatch re-reads the bytes about to be written and checks that
// every target holds exactly the requested value (or is gone, for a delete)
// and that everything outside the targets reads the same as before.
func verifyConfigYAMLPatch(before, after []byte, paths [][]string, deletes, merged []bool, values []*yaml.Node) error {
	var original, updated any
	if err := yaml.Unmarshal(before, &original); err != nil {
		return fmt.Errorf("read current config: %w", err)
	}
	if err := yaml.Unmarshal(after, &updated); err != nil {
		return fmt.Errorf("read updated config: %w", err)
	}
	for index, path := range paths {
		got, found := configValueAt(updated, path)
		if deletes[index] {
			if found {
				return fmt.Errorf("operation %d: %s was not deleted", index, strings.Join(path, "."))
			}
			continue
		}
		var want any
		if err := values[index].Decode(&want); err != nil {
			return fmt.Errorf("operation %d: decode value: %w", index, err)
		}
		if !found || !configValueMatches(want, got, merged[index]) {
			return fmt.Errorf("operation %d: %s does not read back as the requested value", index, strings.Join(path, "."))
		}
	}
	for _, path := range paths {
		// A target may have created parent mappings; drop from the first new one.
		prefix := path
		for length := 1; length <= len(path); length++ {
			if _, found := configValueAt(original, path[:length]); !found {
				prefix = path[:length]
				break
			}
		}
		configDeleteValueAt(original, prefix)
		configDeleteValueAt(updated, prefix)
	}
	if !reflect.DeepEqual(original, updated) {
		return fmt.Errorf("a section outside the requested paths changed")
	}
	return nil
}

// configValueMatches compares a requested value with the stored one. A merged
// mapping only has to contain the requested keys.
func configValueMatches(want, got any, merge bool) bool {
	if !merge {
		return reflect.DeepEqual(want, got)
	}
	wantMap, okWant := want.(map[string]any)
	gotMap, okGot := got.(map[string]any)
	if !okWant || !okGot {
		return reflect.DeepEqual(want, got)
	}
	for key, value := range wantMap {
		stored, found := gotMap[key]
		if !found || !configValueMatches(value, stored, true) {
			return false
		}
	}
	return true
}

// configValuesEquivalent compares a JSON-sourced value with a YAML-sourced
// one. JSON cannot tell 1 from 1.0, so a float equals an integer of the same
// value; two integers must match exactly, which catches precision loss.
func configValuesEquivalent(a, b any) bool {
	if ai, ok := configInteger(a); ok {
		if bi, okB := configInteger(b); okB {
			return ai.Cmp(bi) == 0
		}
	}
	if af, ok := configNumber(a); ok {
		bf, okB := configNumber(b)
		return okB && af == bf
	}
	switch left := a.(type) {
	case []any:
		right, ok := b.([]any)
		if !ok || len(left) != len(right) {
			return false
		}
		for index := range left {
			if !configValuesEquivalent(left[index], right[index]) {
				return false
			}
		}
		return true
	case map[string]any, map[any]any:
		leftMap, rightMap := configStringKeyMap(a), configStringKeyMap(b)
		if rightMap == nil || len(leftMap) != len(rightMap) {
			return false
		}
		for key, value := range leftMap {
			other, found := rightMap[key]
			if !found || !configValuesEquivalent(value, other) {
				return false
			}
		}
		return true
	}
	return reflect.DeepEqual(a, b)
}

func configInteger(value any) (*big.Int, bool) {
	switch number := value.(type) {
	case int:
		return big.NewInt(int64(number)), true
	case int64:
		return big.NewInt(number), true
	case uint64:
		return new(big.Int).SetUint64(number), true
	}
	return nil, false
}

func configNumber(value any) (float64, bool) {
	switch number := value.(type) {
	case int:
		return float64(number), true
	case int64:
		return float64(number), true
	case uint64:
		return float64(number), true
	case float64:
		return number, true
	}
	return 0, false
}

func configStringKeyMap(value any) map[string]any {
	switch mapping := value.(type) {
	case map[string]any:
		return mapping
	case map[any]any:
		out := make(map[string]any, len(mapping))
		for key, item := range mapping {
			out[fmt.Sprint(key)] = item
		}
		return out
	}
	return nil
}

func configValueAt(root any, path []string) (any, bool) {
	current := root
	for _, part := range path {
		switch mapping := current.(type) {
		case map[string]any:
			next, found := mapping[part]
			if !found {
				return nil, false
			}
			current = next
		case map[any]any:
			next, found := configAnyKeyValue(mapping, part)
			if !found {
				return nil, false
			}
			current = next
		default:
			return nil, false
		}
	}
	return current, true
}

func configAnyKeyValue(mapping map[any]any, key string) (any, bool) {
	for candidate, value := range mapping {
		if fmt.Sprint(candidate) == key {
			return value, true
		}
	}
	return nil, false
}

func configDeleteValueAt(root any, path []string) {
	if len(path) == 0 {
		return
	}
	parent, found := configValueAt(root, path[:len(path)-1])
	if !found {
		return
	}
	key := path[len(path)-1]
	switch mapping := parent.(type) {
	case map[string]any:
		delete(mapping, key)
	case map[any]any:
		for candidate := range mapping {
			if fmt.Sprint(candidate) == key {
				delete(mapping, candidate)
			}
		}
	}
}

func configYAMLMappingValue(mapping *yaml.Node, key string) *yaml.Node {
	index := indexYAMLMappingValue(mapping, key)
	if index < 0 || index+1 >= len(mapping.Content) {
		return nil
	}
	return mapping.Content[index+1]
}

func indexYAMLMappingValue(mapping *yaml.Node, key string) int {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return -1
	}
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == key {
			return index
		}
	}
	return -1
}

func configYAMLDeleteMappingValue(mapping *yaml.Node, key string) bool {
	index := indexYAMLMappingValue(mapping, key)
	if index < 0 {
		return false
	}
	mapping.Content = append(mapping.Content[:index], mapping.Content[index+2:]...)
	return true
}

func mergeConfigYAMLMapping(destination, patch *yaml.Node) {
	for index := 0; index+1 < len(patch.Content); index += 2 {
		key := patch.Content[index].Value
		current := configYAMLMappingValue(destination, key)
		updated := patch.Content[index+1]
		if current == nil {
			destination.Content = append(destination.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, updated)
		} else if current.Kind == yaml.MappingNode && updated.Kind == yaml.MappingNode {
			mergeConfigYAMLMapping(current, updated)
		} else {
			updated.HeadComment, updated.LineComment, updated.FootComment = current.HeadComment, current.LineComment, current.FootComment
			destination.Content[indexYAMLMappingValue(destination, key)+1] = updated
		}
	}
}

// GetConfigYAML returns the raw config.yaml file bytes without re-encoding.
// It preserves comments and original formatting/styles.
func (h *Handler) GetConfigYAML(c *gin.Context) {
	data, err := os.ReadFile(h.configFilePath)
	if err != nil {
		if os.IsNotExist(err) {
			c.JSON(http.StatusNotFound, gin.H{"error": "not_found", "message": "config file not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "read_failed", "message": err.Error()})
		return
	}
	c.Header("Content-Type", "application/yaml; charset=utf-8")
	c.Header("Cache-Control", "no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	// The panel sends this back as If-Match on save; without it every write
	// is blind last-write-wins across tabs and operators.
	c.Header("ETag", configETag(data))
	// Write raw bytes as-is
	_, _ = c.Writer.Write(data)
}

// Debug
func (h *Handler) GetDebug(c *gin.Context) { c.JSON(200, gin.H{"debug": h.cfg.Debug}) }
func (h *Handler) PutDebug(c *gin.Context) { h.updateBoolField(c, func(v bool) { h.cfg.Debug = v }) }

// UsageStatisticsEnabled
func (h *Handler) GetUsageStatisticsEnabled(c *gin.Context) {
	c.JSON(200, gin.H{"usage-statistics-enabled": h.cfg.UsageStatisticsEnabled})
}
func (h *Handler) PutUsageStatisticsEnabled(c *gin.Context) {
	h.updateBoolField(c, func(v bool) { h.cfg.UsageStatisticsEnabled = v })
}

// UsageStatisticsEnabled
func (h *Handler) GetLoggingToFile(c *gin.Context) {
	c.JSON(200, gin.H{"logging-to-file": h.cfg.LoggingToFile})
}
func (h *Handler) PutLoggingToFile(c *gin.Context) {
	h.updateBoolField(c, func(v bool) { h.cfg.LoggingToFile = v })
}

// LogsMaxTotalSizeMB
func (h *Handler) GetLogsMaxTotalSizeMB(c *gin.Context) {
	c.JSON(200, gin.H{"logs-max-total-size-mb": h.cfg.LogsMaxTotalSizeMB})
}
func (h *Handler) PutLogsMaxTotalSizeMB(c *gin.Context) {
	var body struct {
		Value *int `json:"value"`
	}
	if errBindJSON := c.ShouldBindJSON(&body); errBindJSON != nil || body.Value == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	value := *body.Value
	if value < 0 {
		value = 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cfg.LogsMaxTotalSizeMB = value
	h.persistLocked(c)
}

// ErrorLogsMaxFiles
func (h *Handler) GetErrorLogsMaxFiles(c *gin.Context) {
	c.JSON(200, gin.H{"error-logs-max-files": h.cfg.ErrorLogsMaxFiles})
}
func (h *Handler) PutErrorLogsMaxFiles(c *gin.Context) {
	var body struct {
		Value *int `json:"value"`
	}
	if errBindJSON := c.ShouldBindJSON(&body); errBindJSON != nil || body.Value == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	value := *body.Value
	if value < 0 {
		value = 10
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cfg.ErrorLogsMaxFiles = value
	h.persistLocked(c)
}

// Request log
func (h *Handler) GetRequestLog(c *gin.Context) { c.JSON(200, gin.H{"request-log": h.cfg.RequestLog}) }
func (h *Handler) PutRequestLog(c *gin.Context) {
	h.updateBoolField(c, func(v bool) { h.cfg.RequestLog = v })
}

// Websocket auth
func (h *Handler) GetWebsocketAuth(c *gin.Context) {
	c.JSON(200, gin.H{"ws-auth": h.cfg.WebsocketAuth})
}
func (h *Handler) PutWebsocketAuth(c *gin.Context) {
	h.updateBoolField(c, func(v bool) { h.cfg.WebsocketAuth = v })
}

// Request retry
func (h *Handler) GetRequestRetry(c *gin.Context) {
	c.JSON(200, gin.H{"request-retry": h.cfg.RequestRetry})
}
func (h *Handler) PutRequestRetry(c *gin.Context) {
	h.updateIntField(c, func(v int) { h.cfg.RequestRetry = v })
}

// Max retry credentials
func (h *Handler) GetMaxRetryCredentials(c *gin.Context) {
	c.JSON(200, gin.H{"max-retry-credentials": h.cfg.MaxRetryCredentials})
}
func (h *Handler) PutMaxRetryCredentials(c *gin.Context) {
	h.updateIntField(c, func(v int) { h.cfg.MaxRetryCredentials = v })
}

// Max retry interval
func (h *Handler) GetMaxRetryInterval(c *gin.Context) {
	c.JSON(200, gin.H{"max-retry-interval": h.cfg.MaxRetryInterval})
}
func (h *Handler) PutMaxRetryInterval(c *gin.Context) {
	h.updateIntField(c, func(v int) { h.cfg.MaxRetryInterval = v })
}

// ForceModelPrefix
func (h *Handler) GetForceModelPrefix(c *gin.Context) {
	c.JSON(200, gin.H{"force-model-prefix": h.cfg.ForceModelPrefix})
}
func (h *Handler) PutForceModelPrefix(c *gin.Context) {
	h.updateBoolField(c, func(v bool) { h.cfg.ForceModelPrefix = v })
}

func normalizeRoutingStrategy(strategy string) (string, bool) {
	normalized := strings.ToLower(strings.TrimSpace(strategy))
	switch normalized {
	case "", "round-robin", "roundrobin", "rr":
		return "round-robin", true
	case "weighted-round-robin", "weightedroundrobin", "wrr":
		return "weighted-round-robin", true
	case "fill-first", "fillfirst", "ff":
		return "fill-first", true
	default:
		return "", false
	}
}

// RoutingStrategy
func (h *Handler) GetRoutingStrategy(c *gin.Context) {
	strategy, ok := normalizeRoutingStrategy(h.cfg.Routing.Strategy)
	sticky := h.cfg.Routing.Sticky
	if sticky <= 0 {
		sticky = 1
	}
	if !ok {
		c.JSON(200, gin.H{
			"strategy":             strings.TrimSpace(h.cfg.Routing.Strategy),
			"sticky":               sticky,
			"session-affinity":     h.cfg.Routing.SessionAffinity,
			"session-affinity-ttl": h.cfg.Routing.SessionAffinityTTL,
		})
		return
	}
	c.JSON(200, gin.H{
		"strategy":             strategy,
		"sticky":               sticky,
		"session-affinity":     h.cfg.Routing.SessionAffinity,
		"session-affinity-ttl": h.cfg.Routing.SessionAffinityTTL,
	})
}
func (h *Handler) PutRoutingStrategy(c *gin.Context) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var body struct {
		Value              *string `json:"value"`
		Strategy           *string `json:"strategy"`
		Sticky             *int    `json:"sticky"`
		SessionAffinity    *bool   `json:"session-affinity"`
		SessionAffinityAlt *bool   `json:"session_affinity"`
		SessionAffinityTTL *string `json:"session-affinity-ttl"`
	}
	if errBindJSON := c.ShouldBindJSON(&body); errBindJSON != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body: " + errBindJSON.Error()})
		return
	}
	stratVal := body.Value
	if stratVal == nil {
		stratVal = body.Strategy
	}
	if stratVal != nil {
		normalized, ok := normalizeRoutingStrategy(*stratVal)
		if !ok {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid strategy"})
			return
		}
		h.cfg.Routing.Strategy = normalized
	}
	if body.Sticky != nil {
		if *body.Sticky > 0 {
			h.cfg.Routing.Sticky = *body.Sticky
		} else {
			h.cfg.Routing.Sticky = 1
		}
	}
	if body.SessionAffinity != nil {
		h.cfg.Routing.SessionAffinity = *body.SessionAffinity
	} else if body.SessionAffinityAlt != nil {
		h.cfg.Routing.SessionAffinity = *body.SessionAffinityAlt
	}
	if body.SessionAffinityTTL != nil {
		h.cfg.Routing.SessionAffinityTTL = strings.TrimSpace(*body.SessionAffinityTTL)
	}
	h.persistLocked(c)
}

// Provider Routing Overrides
func (h *Handler) GetProviderRouting(c *gin.Context) {
	if h.cfg.Routing.Providers == nil {
		c.JSON(200, gin.H{"providers": map[string]config.ProviderRoutingConfig{}})
		return
	}
	c.JSON(200, gin.H{"providers": h.cfg.Routing.Providers})
}

func (h *Handler) PutProviderRouting(c *gin.Context) {
	h.mu.Lock()
	defer h.mu.Unlock()
	name := strings.ToLower(strings.TrimSpace(c.Param("name")))
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "provider name is required"})
		return
	}

	var body struct {
		Strategy           *string `json:"strategy"`
		Sticky             *int    `json:"sticky"`
		SessionAffinity    *bool   `json:"session-affinity"`
		SessionAffinityAlt *bool   `json:"session_affinity"`
		SessionAffinityTTL *string `json:"session-affinity-ttl"`
	}
	if errBind := c.ShouldBindJSON(&body); errBind != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}

	if h.cfg.Routing.Providers == nil {
		h.cfg.Routing.Providers = make(map[string]config.ProviderRoutingConfig)
	}

	current := h.cfg.Routing.Providers[name]
	if body.Strategy != nil {
		s := strings.TrimSpace(*body.Strategy)
		if s != "" && s != "inherit" {
			norm, ok := normalizeRoutingStrategy(s)
			if !ok {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid strategy"})
				return
			}
			current.Strategy = norm
		} else {
			current.Strategy = ""
		}
	}
	if body.Sticky != nil {
		if *body.Sticky > 0 {
			current.Sticky = *body.Sticky
		} else {
			current.Sticky = 0
		}
	}
	if body.SessionAffinity != nil {
		current.SessionAffinity = body.SessionAffinity
	} else if body.SessionAffinityAlt != nil {
		current.SessionAffinity = body.SessionAffinityAlt
	}
	if body.SessionAffinityTTL != nil {
		current.SessionAffinityTTL = strings.TrimSpace(*body.SessionAffinityTTL)
	}

	// If all empty/nil/zero, delete the provider override
	if current.Strategy == "" && current.Sticky == 0 && current.SessionAffinity == nil && current.SessionAffinityTTL == "" {
		delete(h.cfg.Routing.Providers, name)
	} else {
		h.cfg.Routing.Providers[name] = current
	}

	h.persistLocked(c)
}

func (h *Handler) DeleteProviderRouting(c *gin.Context) {
	h.mu.Lock()
	defer h.mu.Unlock()
	name := strings.ToLower(strings.TrimSpace(c.Param("name")))
	if name != "" && h.cfg.Routing.Providers != nil {
		delete(h.cfg.Routing.Providers, name)
		h.persistLocked(c)
		return
	}
	c.JSON(200, gin.H{"status": "ok"})
}

// Proxy URL
func (h *Handler) GetProxyURL(c *gin.Context) { c.JSON(200, gin.H{"proxy-url": h.cfg.ProxyURL}) }
func (h *Handler) PutProxyURL(c *gin.Context) {
	h.updateStringField(c, func(v string) { h.cfg.ProxyURL = v })
}
func (h *Handler) DeleteProxyURL(c *gin.Context) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cfg.ProxyURL = ""
	h.persistLocked(c)
}
