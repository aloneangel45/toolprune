package main

import (
	"bufio"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aloneangel45/toolprune/pkg/mcp"
)

func main() {
	reader := bufio.NewReaderSize(os.Stdin, 10*1024*1024)
	out := os.Stdout

	tools := buildTools()

	for {
		msg, err := mcp.ReadMessage(reader)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, os.ErrClosed) {
				return
			}
			fmt.Fprintf(os.Stderr, "[testserver] read error: %v\n", err)
			return
		}
		if msg == nil {
			continue
		}

		if msg.IsNotification() {
			handleNotification(msg)
			continue
		}

		if msg.IsRequest() {
			resp := handleRequest(msg, tools)
			if resp != nil {
				if err := mcp.WriteMessage(out, resp); err != nil {
					fmt.Fprintf(os.Stderr, "[testserver] write error: %v\n", err)
					return
				}
			}
		}
	}
}

func handleNotification(msg *mcp.Message) {
	switch msg.Method {
	case mcp.MethodNotificationsInitialized:
		fmt.Fprintf(os.Stderr, "[testserver] received notifications/initialized\n")
	case "notifications/cancelled":
		fmt.Fprintf(os.Stderr, "[testserver] received notifications/cancelled\n")
	default:
		fmt.Fprintf(os.Stderr, "[testserver] received generic notification: %s\n", msg.Method)
	}
}

func handleRequest(msg *mcp.Message, tools []mcp.Tool) *mcp.Message {
	switch msg.Method {
	case mcp.MethodInitialize:
		initResult := mcp.InitializeResult{
			ProtocolVersion: mcp.LatestProtocolVersion,
			ServerInfo: mcp.Implementation{
				Name:    "ZeroMockTestServer",
				Version: "1.0.0",
			},
			Capabilities: mcp.ServerCapabilities{
				Tools: &mcp.ToolsCapability{
					ListChanged: true,
				},
			},
			Instructions: "Zero-Mock Real Subprocess MCP Server for ToolPrune E2E Testing. Exposes realistic tools with genuine computation.",
		}
		resp, _ := mcp.NewResponse(*msg.ID, initResult)
		return resp

	case mcp.MethodPing:
		resp, _ := mcp.NewResponse(*msg.ID, map[string]any{})
		return resp

	case mcp.MethodToolsList:
		listResult := mcp.ListToolsResult{
			Tools: tools,
		}
		resp, _ := mcp.NewResponse(*msg.ID, listResult)
		return resp

	case mcp.MethodToolsCall:
		var callParams mcp.CallToolRequestParams
		if err := json.Unmarshal(msg.Params, &callParams); err != nil {
			return mcp.NewErrorMessage(msg.ID, mcp.CodeInvalidParams, fmt.Sprintf("invalid call params: %v", err))
		}

		result, isErr := executeToolCall(callParams.Name, callParams.Arguments)
		toolResult := mcp.CallToolResult{
			Content: []mcp.ContentItem{
				{
					Type: "text",
					Text: result,
				},
			},
			IsError: isErr,
		}
		resp, _ := mcp.NewResponse(*msg.ID, toolResult)
		return resp

	default:
		return mcp.NewErrorMessage(msg.ID, mcp.CodeMethodNotFound, fmt.Sprintf("method %q not found", msg.Method))
	}
}

func executeToolCall(name string, rawArgs json.RawMessage) (string, bool) {
	var args map[string]any
	if len(rawArgs) > 0 {
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return fmt.Sprintf("JSON unmarshal error for arguments: %v", err), true
		}
	} else {
		args = make(map[string]any)
	}

	switch name {
	case "math_eval":
		return executeMathEval(args)
	case "string_transform":
		return executeStringTransform(args)
	case "system_info":
		return executeSystemInfo(args)
	case "data_query":
		return executeDataQuery(args)
	case "cloud_config_validator":
		return executeCloudConfigValidator(args)
	case "database_schema_analyzer":
		return executeDatabaseSchemaAnalyzer(args)
	case "file_inspect":
		return executeFileInspect(args)
	case "kubernetes_manifest_linter":
		return executeKubernetesManifestLinter(args)
	default:
		return fmt.Sprintf("unknown tool %q", name), true
	}
}

// -----------------------------------------------------------------------------
// Real Tool Implementations (Zero Mock)
// -----------------------------------------------------------------------------

func executeMathEval(args map[string]any) (string, bool) {
	op, _ := args["operation"].(string)
	if op == "" {
		return "missing required parameter 'operation'", true
	}

	toNum := func(v any) (float64, bool) {
		switch n := v.(type) {
		case float64:
			return n, true
		case int:
			return float64(n), true
		case int64:
			return float64(n), true
		case string:
			f, err := strconv.ParseFloat(n, 64)
			return f, err == nil
		default:
			return 0, false
		}
	}

	a, _ := toNum(args["a"])
	b, _ := toNum(args["b"])

	precision := 6
	if p, ok := toNum(args["precision"]); ok && p >= 0 && p <= 10 {
		precision = int(p)
	}

	roundVal := func(val float64) float64 {
		pow := math.Pow(10, float64(precision))
		return math.Round(val*pow) / pow
	}

	switch strings.ToLower(op) {
	case "add":
		res := roundVal(a + b)
		return fmt.Sprintf("%v", res), false
	case "subtract":
		res := roundVal(a - b)
		return fmt.Sprintf("%v", res), false
	case "multiply":
		res := roundVal(a * b)
		return fmt.Sprintf("%v", res), false
	case "divide":
		if b == 0 {
			return "division by zero error", true
		}
		res := roundVal(a / b)
		return fmt.Sprintf("%v", res), false
	case "power":
		res := roundVal(math.Pow(a, b))
		return fmt.Sprintf("%v", res), false
	case "sqrt":
		if a < 0 {
			return "cannot compute square root of negative number", true
		}
		res := roundVal(math.Sqrt(a))
		return fmt.Sprintf("%v", res), false
	case "modulo":
		if b == 0 {
			return "modulo by zero error", true
		}
		res := roundVal(math.Mod(a, b))
		return fmt.Sprintf("%v", res), false
	case "factorial":
		if a < 0 || a > 20 || math.Floor(a) != a {
			return "factorial requires an integer between 0 and 20", true
		}
		n := int64(a)
		fact := int64(1)
		for i := int64(2); i <= n; i++ {
			fact *= i
		}
		return fmt.Sprintf("%d", fact), false
	case "sum", "mean", "median", "stddev", "min", "max":
		rawList, ok := args["numbers"].([]any)
		if !ok || len(rawList) == 0 {
			return fmt.Sprintf("operation %q requires a non-empty array 'numbers'", op), true
		}
		nums := make([]float64, 0, len(rawList))
		for _, item := range rawList {
			if val, ok := toNum(item); ok {
				nums = append(nums, val)
			}
		}
		if len(nums) == 0 {
			return "no valid numeric values provided in 'numbers'", true
		}

		sum := 0.0
		minVal := nums[0]
		maxVal := nums[0]
		for _, n := range nums {
			sum += n
			if n < minVal {
				minVal = n
			}
			if n > maxVal {
				maxVal = n
			}
		}

		switch strings.ToLower(op) {
		case "sum":
			return fmt.Sprintf("%v", roundVal(sum)), false
		case "min":
			return fmt.Sprintf("%v", roundVal(minVal)), false
		case "max":
			return fmt.Sprintf("%v", roundVal(maxVal)), false
		case "mean":
			mean := sum / float64(len(nums))
			return fmt.Sprintf("%v", roundVal(mean)), false
		case "median":
			sort.Float64s(nums)
			mid := len(nums) / 2
			var med float64
			if len(nums)%2 == 0 {
				med = (nums[mid-1] + nums[mid]) / 2.0
			} else {
				med = nums[mid]
			}
			return fmt.Sprintf("%v", roundVal(med)), false
		case "stddev":
			mean := sum / float64(len(nums))
			varianceSum := 0.0
			for _, n := range nums {
				diff := n - mean
				varianceSum += diff * diff
			}
			stddev := math.Sqrt(varianceSum / float64(len(nums)))
			return fmt.Sprintf("%v", roundVal(stddev)), false
		}
	}

	return fmt.Sprintf("unsupported operation %q", op), true
}

func executeStringTransform(args map[string]any) (string, bool) {
	action, _ := args["action"].(string)
	input, _ := args["input"].(string)
	if action == "" {
		return "missing required parameter 'action'", true
	}

	switch strings.ToLower(action) {
	case "uppercase":
		return strings.ToUpper(input), false
	case "lowercase":
		return strings.ToLower(input), false
	case "titlecase":
		return strings.Title(strings.ToLower(input)), false
	case "reverse":
		runes := []rune(input)
		for i, j := 0, len(runes)-1; i < j; i, j = i+1, j-1 {
			runes[i], runes[j] = runes[j], runes[i]
		}
		return string(runes), false
	case "base64_encode":
		return base64.StdEncoding.EncodeToString([]byte(input)), false
	case "base64_decode":
		decoded, err := base64.StdEncoding.DecodeString(input)
		if err != nil {
			return fmt.Sprintf("invalid base64 payload: %v", err), true
		}
		return string(decoded), false
	case "sha256":
		h := sha256.Sum256([]byte(input))
		return hex.EncodeToString(h[:]), false
	case "md5":
		h := md5.Sum([]byte(input))
		return hex.EncodeToString(h[:]), false
	case "word_count":
		words := strings.Fields(input)
		return fmt.Sprintf("%d", len(words)), false
	case "trim":
		return strings.TrimSpace(input), false
	case "slugify":
		clean := strings.ToLower(input)
		reg := regexp.MustCompile(`[^a-z0-9]+`)
		slug := reg.ReplaceAllString(clean, "-")
		slug = strings.Trim(slug, "-")
		return slug, false
	case "regex_replace":
		pat, _ := args["pattern"].(string)
		repl, _ := args["replacement"].(string)
		if pat == "" {
			return "missing pattern for regex_replace", true
		}
		re, err := regexp.Compile(pat)
		if err != nil {
			return fmt.Sprintf("invalid regex pattern %q: %v", pat, err), true
		}
		return re.ReplaceAllString(input, repl), false
	default:
		return fmt.Sprintf("unsupported action %q", action), true
	}
}

func executeSystemInfo(args map[string]any) (string, bool) {
	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)

	info := map[string]any{
		"go_version": runtime.Version(),
		"os":         runtime.GOOS,
		"arch":       runtime.GOARCH,
		"num_cpu":    runtime.NumCPU(),
		"pid":        os.Getpid(),
		"alloc_mb":   fmt.Sprintf("%.2f MB", float64(memStats.Alloc)/1024/1024),
		"sys_mb":     fmt.Sprintf("%.2f MB", float64(memStats.Sys)/1024/1024),
		"num_gc":     memStats.NumGC,
		"timestamp":  time.Now().UTC().Format(time.RFC3339),
	}

	b, _ := json.MarshalIndent(info, "", "  ")
	return string(b), false
}

func executeDataQuery(args map[string]any) (string, bool) {
	rawDataset, ok := args["dataset"].([]any)
	if !ok || len(rawDataset) == 0 {
		return "missing or empty 'dataset' array", true
	}

	filterField, _ := args["filter_field"].(string)
	filterOp, _ := args["filter_operator"].(string)
	filterVal := args["filter_value"]

	var filtered []map[string]any
	for _, item := range rawDataset {
		rec, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if filterField == "" || filterOp == "" {
			filtered = append(filtered, rec)
			continue
		}

		val, exists := rec[filterField]
		if !exists {
			continue
		}

		match := false
		switch strings.ToLower(filterOp) {
		case "eq":
			match = fmt.Sprintf("%v", val) == fmt.Sprintf("%v", filterVal)
		case "neq":
			match = fmt.Sprintf("%v", val) != fmt.Sprintf("%v", filterVal)
		case "contains":
			match = strings.Contains(fmt.Sprintf("%v", val), fmt.Sprintf("%v", filterVal))
		case "startswith":
			match = strings.HasPrefix(fmt.Sprintf("%v", val), fmt.Sprintf("%v", filterVal))
		case "endswith":
			match = strings.HasSuffix(fmt.Sprintf("%v", val), fmt.Sprintf("%v", filterVal))
		case "gt", "gte", "lt", "lte":
			valNum, ok1 := toFloat(val)
			filterNum, ok2 := toFloat(filterVal)
			if ok1 && ok2 {
				switch filterOp {
				case "gt":
					match = valNum > filterNum
				case "gte":
					match = valNum >= filterNum
				case "lt":
					match = valNum < filterNum
				case "lte":
					match = valNum <= filterNum
				}
			}
		}
		if match {
			filtered = append(filtered, rec)
		}
	}

	// Limit and Offset
	offset := 0
	if off, ok := toFloat(args["offset"]); ok && off > 0 {
		offset = int(off)
	}
	limit := len(filtered)
	if lim, ok := toFloat(args["limit"]); ok && lim > 0 {
		limit = int(lim)
	}

	if offset >= len(filtered) {
		filtered = []map[string]any{}
	} else {
		end := offset + limit
		if end > len(filtered) {
			end = len(filtered)
		}
		filtered = filtered[offset:end]
	}

	out, _ := json.Marshal(filtered)
	return string(out), false
}

func executeCloudConfigValidator(args map[string]any) (string, bool) {
	provider, _ := args["provider"].(string)
	if provider == "" {
		return "missing required parameter 'provider'", true
	}
	region, _ := args["region"].(string)
	if region == "" {
		return "missing required parameter 'region'", true
	}

	violations := []string{}
	vpc, _ := args["vpc"].(map[string]any)
	if vpc != nil {
		cidr, _ := vpc["cidr_block"].(string)
		if cidr != "" {
			_, _, err := net.ParseCIDR(cidr)
			if err != nil {
				violations = append(violations, fmt.Sprintf("invalid VPC CIDR block format %q: %v", cidr, err))
			}
		}
	}

	subnets, _ := args["subnets"].([]any)
	for i, s := range subnets {
		subMap, ok := s.(map[string]any)
		if !ok {
			continue
		}
		cidr, _ := subMap["cidr_block"].(string)
		if cidr != "" {
			_, _, err := net.ParseCIDR(cidr)
			if err != nil {
				violations = append(violations, fmt.Sprintf("subnet[%d] invalid CIDR %q", i, cidr))
			}
		}
	}

	report := map[string]any{
		"provider":         provider,
		"region":           region,
		"valid":            len(violations) == 0,
		"violations_count": len(violations),
		"violations":       violations,
		"framework":        args["compliance_framework"],
		"timestamp":        time.Now().UTC().Format(time.RFC3339),
	}
	out, _ := json.MarshalIndent(report, "", "  ")
	return string(out), false
}

func executeDatabaseSchemaAnalyzer(args map[string]any) (string, bool) {
	engine, _ := args["engine"].(string)
	if engine == "" {
		return "missing required parameter 'engine'", true
	}

	tables, _ := args["tables"].([]any)
	if len(tables) == 0 {
		return "missing or empty 'tables' array", true
	}

	suggestions := []string{}
	tableStats := make([]map[string]any, 0, len(tables))

	for _, t := range tables {
		tbl, ok := t.(map[string]any)
		if !ok {
			continue
		}
		name, _ := tbl["table_name"].(string)
		cols, _ := tbl["columns"].([]any)
		hasPK := false
		for _, c := range cols {
			colMap, ok := c.(map[string]any)
			if !ok {
				continue
			}
			if isPK, _ := colMap["is_primary_key"].(bool); isPK {
				hasPK = true
				break
			}
		}
		if !hasPK {
			suggestions = append(suggestions, fmt.Sprintf("Table %q lacks an explicit primary key constraint.", name))
		}
		tableStats = append(tableStats, map[string]any{
			"table":        name,
			"column_count": len(cols),
			"has_pk":       hasPK,
		})
	}

	result := map[string]any{
		"engine":       engine,
		"total_tables": len(tables),
		"table_stats":  tableStats,
		"suggestions":  suggestions,
		"normalized":   len(suggestions) == 0,
	}
	out, _ := json.MarshalIndent(result, "", "  ")
	return string(out), false
}

func executeFileInspect(args map[string]any) (string, bool) {
	content, _ := args["content"].(string)
	filename, _ := args["filename"].(string)

	lines := strings.Split(content, "\n")
	words := strings.Fields(content)

	sha256Hex := fmt.Sprintf("%x", sha256.Sum256([]byte(content)))
	sha1Hex := fmt.Sprintf("%x", sha1.Sum([]byte(content)))
	md5Hex := fmt.Sprintf("%x", md5.Sum([]byte(content)))

	res := map[string]any{
		"filename":    filename,
		"byte_length": len(content),
		"rune_length": len([]rune(content)),
		"line_count":  len(lines),
		"word_count":  len(words),
		"sha256":      sha256Hex,
		"sha1":        sha1Hex,
		"md5":         md5Hex,
		"is_utf8":     json.Valid([]byte(`"` + strings.ReplaceAll(content, `"`, `\"`) + `"`)),
	}
	out, _ := json.MarshalIndent(res, "", "  ")
	return string(out), false
}

func executeKubernetesManifestLinter(args map[string]any) (string, bool) {
	kind, _ := args["kind"].(string)
	if kind == "" {
		return "missing required parameter 'kind'", true
	}
	manifest, _ := args["manifest"].(string)

	findings := []string{}
	secContext, _ := args["security_context"].(map[string]any)
	if secContext != nil {
		if runAsNonRoot, ok := secContext["run_as_non_root"].(bool); !ok || !runAsNonRoot {
			findings = append(findings, "SecurityContext violation: runAsNonRoot must be set to true")
		}
		if readOnlyFS, ok := secContext["read_only_root_filesystem"].(bool); !ok || !readOnlyFS {
			findings = append(findings, "SecurityContext violation: readOnlyRootFilesystem must be true")
		}
		if allowPrivEsc, ok := secContext["allow_privilege_escalation"].(bool); ok && allowPrivEsc {
			findings = append(findings, "SecurityContext violation: allowPrivilegeEscalation must be false")
		}
	} else {
		findings = append(findings, "Missing securityContext block on workload specification")
	}

	result := map[string]any{
		"kind":          kind,
		"manifest_size": len(manifest),
		"compliant":     len(findings) == 0,
		"findings":      findings,
		"benchmark":     "NSA/CISA Kubernetes Hardening Guidance v1.2",
	}
	out, _ := json.MarshalIndent(result, "", "  ")
	return string(out), false
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case string:
		f, err := strconv.ParseFloat(n, 64)
		return f, err == nil
	default:
		return 0, false
	}
}

// -----------------------------------------------------------------------------
// Comprehensive Verbose Tool Schemas (> 3,500 unpruned tokens)
// -----------------------------------------------------------------------------

func buildTools() []mcp.Tool {
	return []mcp.Tool{
		{
			Name: "math_eval",
			Description: `High-precision enterprise mathematical computation engine designed for deterministic numeric operations, statistical data analysis, scientific computing, matrix reductions, and combinatorial logic.

### Supported Operations
- Arithmetic: addition, subtraction, multiplication, division, modulo, and exponentiation.
- Statistics: comprehensive population and sample statistics over dynamic arrays including mean, median, sample variance, standard deviation, and bounding extrema (min/max/sum).
- Combinatorics: factorial calculations with overflow guardrails up to 20!

### Numeric Formatting and Rounding
Supports configurable floating-point precision from 0 to 10 decimal digits with IEEE-754 half-up rounding semantics. Returns clean string-formatted decimal numbers.`,
			InputSchema: mcp.InputSchema{
				Schema:               "http://json-schema.org/draft-07/schema#",
				Type:                 "object",
				AdditionalProperties: false,
				Properties: map[string]any{
					"operation": map[string]any{
						"type":        "string",
						"description": "The exact mathematical operation to execute on the operands. Must be one of the supported arithmetic or statistical operators.",
						"enum": []string{
							"add", "subtract", "multiply", "divide", "power", "sqrt",
							"modulo", "factorial", "sum", "mean", "median", "stddev", "min", "max",
						},
					},
					"a": map[string]any{
						"type":        "number",
						"description": "Primary numeric operand 'a' used for scalar operations (add, subtract, multiply, divide, power, sqrt, modulo, factorial).",
					},
					"b": map[string]any{
						"type":        "number",
						"description": "Secondary numeric operand 'b' used for binary scalar operations (divisor, exponent, subtrahend, modulus).",
					},
					"numbers": map[string]any{
						"type":        "array",
						"description": "An ordered list of IEEE-754 64-bit floating point numbers to aggregate when performing statistical operations (sum, mean, median, stddev, min, max).",
						"items": map[string]any{
							"type":        "number",
							"description": "Individual sample value within the data series.",
						},
					},
					"precision": map[string]any{
						"type":        "integer",
						"description": "Number of fractional decimal places to retain in the formatted floating point result. Defaults to 6 digits (min 0, max 10).",
						"minimum":     0,
						"maximum":     10,
					},
					"round_mode": map[string]any{
						"type":        "string",
						"description": "Rounding algorithm applied when truncating fractional components exceeding the specified precision.",
						"enum":        []string{"nearest", "floor", "ceil", "trunc"},
					},
					"verbose_output": map[string]any{
						"type":        "boolean",
						"description": "When true, includes calculation metadata, duration in microseconds, and operand verification diagnostics alongside the numeric result.",
					},
				},
				Required: []string{"operation"},
			},
		},
		{
			Name: "string_transform",
			Description: `Production text processing and cryptographic hashing pipeline supporting multi-stage transformations, regular expression substitution, casing normalizations, RFC 4648 Base64 encoding/decoding, and cryptographic message digests.

### Transformation Capabilities
1. Casing Normalization: converts unstructured strings into uppercase, lowercase, title case, or URL-safe slug formats.
2. Inversion & Padding: performs Unicode rune-aware string reversal, bidirectional whitespace trimming, and length normalization.
3. Cryptographic Digests: computes secure NIST-compliant SHA-256 and MD5 hexadecimal message digests.
4. Regular Expression Engine: applies POSIX regular expression search-and-replace patterns with captured group references.`,
			InputSchema: mcp.InputSchema{
				Schema:               "http://json-schema.org/draft-07/schema#",
				Type:                 "object",
				AdditionalProperties: false,
				Properties: map[string]any{
					"action": map[string]any{
						"type":        "string",
						"description": "The target text transformation or cryptographic hashing operation to perform on the input text.",
						"enum": []string{
							"uppercase", "lowercase", "titlecase", "reverse",
							"base64_encode", "base64_decode", "sha256", "md5",
							"word_count", "trim", "slugify", "regex_replace",
						},
					},
					"input": map[string]any{
						"type":        "string",
						"description": "The source UTF-8 text string to be transformed, hashed, or searched.",
					},
					"pattern": map[string]any{
						"type":        "string",
						"description": "Regular expression pattern compiled using Go regexp syntax. Used exclusively when action is 'regex_replace'. Example: '[^a-zA-Z0-9]+'.",
					},
					"replacement": map[string]any{
						"type":        "string",
						"description": "The substitution string used to replace pattern occurrences during 'regex_replace' operations. Supports expansion syntax like '$1'.",
					},
					"options": map[string]any{
						"type":        "object",
						"description": "Fine-grained behavioral configuration flags governing regular expression execution and whitespace sanitization.",
						"properties": map[string]any{
							"case_sensitive": map[string]any{
								"type":        "boolean",
								"description": "Enforces case sensitivity during pattern matching and token identification.",
							},
							"multiline": map[string]any{
								"type":        "boolean",
								"description": "Enables multiline mode where '^' and '$' match the start and end of individual lines.",
							},
							"collapse_whitespace": map[string]any{
								"type":        "boolean",
								"description": "Consolidates adjacent whitespace runes and newline sequences into single spaces before processing.",
							},
						},
					},
				},
				Required: []string{"action", "input"},
			},
		},
		{
			Name: "system_info",
			Description: `Hardware and runtime diagnostics collector providing real-time introspection into the host operating system, Go execution environment, memory allocations, garbage collection metrics, and hardware concurrency limits.

### Telemetry Domains
- Go Compiler & Runtime: reports Go version, target GOOS, target GOARCH, and active logical CPU core count.
- Virtual Memory & Heap: queries live runtime.MemStats to capture total allocated bytes, heap allocations, and garbage collector invocations.
- Host Process Boundary: returns current process PID and synchronized UTC timestamp in ISO 8601 / RFC 3339 format.`,
			InputSchema: mcp.InputSchema{
				Schema:               "http://json-schema.org/draft-07/schema#",
				Type:                 "object",
				AdditionalProperties: false,
				Properties: map[string]any{
					"sections": map[string]any{
						"type":        "array",
						"description": "Specific telemetry categories to retrieve. Passing 'all' returns the comprehensive diagnostic bundle.",
						"items": map[string]any{
							"type": "string",
							"enum": []string{"runtime", "memory", "os", "env", "all"},
						},
					},
					"include_environment": map[string]any{
						"type":        "boolean",
						"description": "Whether to query the host process environment variable table. Sensitive tokens are masked.",
					},
					"filter_env_prefix": map[string]any{
						"type":        "string",
						"description": "Optional environment variable prefix filter (e.g. 'TOOLPRUNE_' or 'GO_') to restrict returned variables.",
					},
					"format": map[string]any{
						"type":        "string",
						"description": "Desired serialization structure for telemetry diagnostics.",
						"enum":        []string{"json", "text", "summary"},
					},
				},
			},
		},
		{
			Name: "data_query",
			Description: `Declarative query engine for in-memory JSON document collections. Features multi-operator predicate filtering, deep attribute path resolution, multi-field ascending/descending sorting, limit-offset windowing, and field projection.

### Filtering Capabilities
Supports standard comparison operators (eq, neq, gt, gte, lt, lte) and text matching (contains, startswith, endswith). Handles dynamic type casting between numbers, strings, and booleans.

### Pagination & Projection
Enables sliding window pagination across large document datasets with boundary-safe limit and offset parameters, accompanied by sparse field projections to minimize payload size.`,
			InputSchema: mcp.InputSchema{
				Schema:               "http://json-schema.org/draft-07/schema#",
				Type:                 "object",
				AdditionalProperties: false,
				Properties: map[string]any{
					"dataset": map[string]any{
						"type":        "array",
						"description": "The array of JSON record objects to evaluate against filter criteria and sort specifications.",
						"items": map[string]any{
							"type": "object",
						},
					},
					"filter_field": map[string]any{
						"type":        "string",
						"description": "Top-level property key name in each document record to evaluate against the filter condition.",
					},
					"filter_operator": map[string]any{
						"type":        "string",
						"description": "Comparison operator applied between the record field value and filter_value.",
						"enum":        []string{"eq", "neq", "gt", "gte", "lt", "lte", "contains", "startswith", "endswith"},
					},
					"filter_value": map[string]any{
						"description": "Target comparison operand (primitive string, number, or boolean).",
					},
					"sort_field": map[string]any{
						"type":        "string",
						"description": "Optional attribute name used as the primary sorting key.",
					},
					"sort_direction": map[string]any{
						"type":        "string",
						"description": "Sorting direction for the query results.",
						"enum":        []string{"asc", "desc"},
					},
					"limit": map[string]any{
						"type":        "integer",
						"description": "Maximum number of records to return in the queried result set (must be >= 1).",
						"minimum":     1,
					},
					"offset": map[string]any{
						"type":        "integer",
						"description": "Number of matching initial records to bypass before collecting the result window (for pagination).",
						"minimum":     0,
					},
					"project_fields": map[string]any{
						"type":        "array",
						"description": "Optional whitelist of attribute keys to retain in the resulting records. Omitted attributes are stripped.",
						"items": map[string]any{
							"type": "string",
						},
					},
				},
				Required: []string{"dataset"},
			},
		},
		{
			Name: "cloud_config_validator",
			Description: `Enterprise cloud infrastructure and virtual private cloud (VPC) network topology validator. Audits AWS, GCP, and Azure configurations against CIS Security Benchmarks, RFC 1918 private IP allocation rules, and least-privilege security group policies.

### Validation Matrix
- VPC CIDR Masking: verifies valid IPv4 CIDR notation and checks for recommended prefix sizes (/16 to /24).
- Subnet Allocation: checks that subnets fall within VPC boundaries and assigns proper public/private route tables.
- Security Groups: audits ingress and egress rules to detect dangerous open access (0.0.0.0/0 on SSH port 22 or RDP port 3389).
- Compliance Frameworks: evaluates configurations against CIS AWS Foundations Benchmark, PCI-DSS v3.2, HIPAA, and SOC2 standards.`,
			InputSchema: mcp.InputSchema{
				Schema:               "http://json-schema.org/draft-07/schema#",
				Type:                 "object",
				AdditionalProperties: false,
				Properties: map[string]any{
					"provider": map[string]any{
						"type":        "string",
						"description": "Cloud service provider hosting the target infrastructure.",
						"enum":        []string{"aws", "gcp", "azure"},
					},
					"region": map[string]any{
						"type":        "string",
						"description": "Target geographic cloud deployment region. Must follow standard region naming schemes (e.g. 'us-east-1', 'eu-west-1', 'ap-southeast-2').",
						"pattern":     "^[a-z]{2}-[a-z]+-\\d{1}$",
					},
					"vpc": map[string]any{
						"type":        "object",
						"description": "Virtual Private Cloud network container definition.",
						"properties": map[string]any{
							"cidr_block": map[string]any{
								"type":        "string",
								"description": "Primary IPv4 Classless Inter-Domain Routing address block allocated to the VPC.",
								"pattern":     "^((25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)\\.){3}(25[0-5]|2[0-4][0-9]|[01]?[0-9][0-9]?)/(1[6-9]|2[0-8])$",
							},
							"enable_dns_hostnames": map[string]any{
								"type":        "boolean",
								"description": "Indicates whether instances launched in the VPC receive public DNS hostnames.",
							},
							"enable_dns_support": map[string]any{
								"type":        "boolean",
								"description": "Indicates whether DNS resolution is enabled through the Amazon Provided DNS server.",
							},
							"tags": map[string]any{
								"type":        "object",
								"description": "Key-value metadata tags assigned to the VPC resource.",
							},
						},
						"required": []string{"cidr_block"},
					},
					"subnets": map[string]any{
						"type":        "array",
						"description": "List of subnet subdivisions configured within the VPC boundary.",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"name": map[string]any{
									"type":        "string",
									"description": "Descriptive identifier for the subnet.",
								},
								"cidr_block": map[string]any{
									"type":        "string",
									"description": "IPv4 CIDR allocation for this subnet.",
								},
								"availability_zone": map[string]any{
									"type":        "string",
									"description": "Target physical availability zone letter (e.g. 'us-east-1a').",
								},
								"is_public": map[string]any{
									"type":        "boolean",
									"description": "When true, subnet routes internet traffic via an Internet Gateway.",
								},
							},
							"required": []string{"name", "cidr_block"},
						},
					},
					"security_groups": map[string]any{
						"type":        "array",
						"description": "Stateful firewall security group definitions controlling inbound and outbound packet filtering.",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"group_name": map[string]any{
									"type":        "string",
									"description": "Unique security group name within the VPC.",
								},
								"description": map[string]any{
									"type":        "string",
									"description": "Purpose and ownership description for this security group.",
								},
								"ingress_rules": map[string]any{
									"type":        "array",
									"description": "List of inbound traffic firewall rules.",
									"items": map[string]any{
										"type": "object",
										"properties": map[string]any{
											"protocol": map[string]any{
												"type": "string",
												"enum": []string{"tcp", "udp", "icmp", "all"},
											},
											"from_port": map[string]any{
												"type":    "integer",
												"minimum": 0,
												"maximum": 65535,
											},
											"to_port": map[string]any{
												"type":    "integer",
												"minimum": 0,
												"maximum": 65535,
											},
											"cidr_blocks": map[string]any{
												"type": "array",
												"items": map[string]any{
													"type": "string",
												},
											},
										},
										"required": []string{"protocol"},
									},
								},
							},
							"required": []string{"group_name"},
						},
					},
					"compliance_framework": map[string]any{
						"type":        "string",
						"description": "Governing cybersecurity compliance standard used to benchmark firewall and encryption settings.",
						"enum":        []string{"cis-v1.4", "pci-dss-v3.2", "hipaa", "soc2"},
					},
					"strict_mode": map[string]any{
						"type":        "boolean",
						"description": "When true, treats any informational security warning as a hard validation failure.",
					},
				},
				Required: []string{"provider", "region", "vpc"},
			},
		},
		{
			Name: "database_schema_analyzer",
			Description: `Automated relational and document database schema inspector, normal form validator, and index optimization advisor. Analyzes SQL table structures, primary keys, foreign key referential integrity, and composite index configurations.

### Diagnostic Audits
1. Normalization Review: checks table definitions for 1NF (atomic columns), 2NF (functional dependency on candidate keys), and 3NF/BCNF compliance.
2. Missing Key Detection: identifies orphan tables lacking explicit primary key constraints or surrogate auto-increment identifiers.
3. Foreign Key Indexing: discovers unindexed foreign key column references that can cause catastrophic full table locks during CASCADE operations.`,
			InputSchema: mcp.InputSchema{
				Schema:               "http://json-schema.org/draft-07/schema#",
				Type:                 "object",
				AdditionalProperties: false,
				Properties: map[string]any{
					"engine": map[string]any{
						"type":        "string",
						"description": "Database management engine dialect used for type compatibility evaluation.",
						"enum":        []string{"postgres", "mysql", "sqlite", "cockroachdb"},
					},
					"tables": map[string]any{
						"type":        "array",
						"description": "Collection of schema table models to inspect.",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"table_name": map[string]any{
									"type":        "string",
									"description": "SQL identifier for the table name.",
								},
								"columns": map[string]any{
									"type":        "array",
									"description": "Column definitions comprising the table schema.",
									"items": map[string]any{
										"type": "object",
										"properties": map[string]any{
											"name": map[string]any{
												"type": "string",
											},
											"type": map[string]any{
												"type": "string",
											},
											"nullable": map[string]any{
												"type": "boolean",
											},
											"is_primary_key": map[string]any{
												"type": "boolean",
											},
											"is_unique": map[string]any{
												"type": "boolean",
											},
											"default_value": map[string]any{
												"type": "string",
											},
										},
										"required": []string{"name", "type"},
									},
								},
								"indexes": map[string]any{
									"type":        "array",
									"description": "Index structures defined on the table.",
									"items": map[string]any{
										"type": "object",
										"properties": map[string]any{
											"name": map[string]any{
												"type": "string",
											},
											"columns": map[string]any{
												"type": "array",
												"items": map[string]any{
													"type": "string",
												},
											},
											"is_unique": map[string]any{
												"type": "boolean",
											},
											"index_type": map[string]any{
												"type": "string",
												"enum": []string{"btree", "hash", "gin", "gist"},
											},
										},
										"required": []string{"name", "columns"},
									},
								},
							},
							"required": []string{"table_name", "columns"},
						},
					},
					"check_normalization": map[string]any{
						"type":        "boolean",
						"description": "Enables algorithmic review of functional dependencies against relational normal forms.",
					},
					"target_normal_form": map[string]any{
						"type":        "string",
						"description": "Target relational normal form to test for compliance.",
						"enum":        []string{"1NF", "2NF", "3NF", "BCNF"},
					},
					"suggest_indexes": map[string]any{
						"type":        "boolean",
						"description": "Generates performance-enhancing index recommendations for high-cardinality foreign keys.",
					},
				},
				Required: []string{"engine", "tables"},
			},
		},
		{
			Name: "file_inspect",
			Description: `Local filesystem content analyzer and cryptographic integrity verification tool. Calculates character counts, line counts, cryptographic hashes (SHA-256, SHA-1, MD5), and validates UTF-8 encoding compliance across source files.

### Metrics Computed
- Structural Metrics: counts raw bytes, Unicode runes, newline-delimited lines, and whitespace-delimited words.
- Cryptographic Hashes: computes simultaneous multi-algorithm hashes without multiple passes over file content.
- Encoding Validation: asserts valid UTF-8 sequence compliance according to RFC 3629.`,
			InputSchema: mcp.InputSchema{
				Schema:               "http://json-schema.org/draft-07/schema#",
				Type:                 "object",
				AdditionalProperties: false,
				Properties: map[string]any{
					"content": map[string]any{
						"type":        "string",
						"description": "Raw file textual content to be analyzed.",
					},
					"filename": map[string]any{
						"type":        "string",
						"description": "Virtual or physical filename representing the source file being inspected.",
					},
					"compute_hashes": map[string]any{
						"type":        "array",
						"description": "Selection of cryptographic digest algorithms to evaluate against file content.",
						"items": map[string]any{
							"type": "string",
							"enum": []string{"sha256", "sha1", "md5"},
						},
					},
					"analyze_syntax": map[string]any{
						"type":        "string",
						"description": "Optional grammar specification used to validate file syntax integrity.",
						"enum":        []string{"go", "json", "yaml", "markdown", "plaintext"},
					},
				},
				Required: []string{"content"},
			},
		},
		{
			Name:        "kubernetes_manifest_linter",
			Description: `Automated static analysis and security benchmarking linter for Cloud Native Computing Foundation (CNCF) Kubernetes workload resources, validating declarative YAML/JSON configurations against CIS Benchmarks, NSA/CISA Hardening guidelines, and Pod Security Standards (Privileged, Baseline, Restricted).`,
			InputSchema: mcp.InputSchema{
				Type: "object",
				Properties: map[string]any{
					"kind": map[string]any{
						"type":        "string",
						"description": "Target Kubernetes resource workload kind to inspect and validate against the security posture matrix.",
						"enum":        []string{"Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob", "Pod"},
					},
					"manifest": map[string]any{
						"type":        "string",
						"description": "Raw UTF-8 declarative manifest text representing the Kubernetes object definition.",
					},
					"target_cluster_version": map[string]any{
						"type":        "string",
						"description": "Target Kubernetes control plane API version used to evaluate deprecated or removed API groups.",
						"enum":        []string{"1.28", "1.29", "1.30", "1.31"},
					},
					"security_context": map[string]any{
						"type":        "object",
						"description": "Container or pod-level security context configuration evaluating privilege escalation, user IDs, and capability drops.",
						"properties": map[string]any{
							"run_as_non_root": map[string]any{
								"type":        "boolean",
								"description": "Indicates that the container must run as a non-root user (UID != 0).",
							},
							"read_only_root_filesystem": map[string]any{
								"type":        "boolean",
								"description": "Whether this container has a read-only root filesystem mounted.",
							},
							"allow_privilege_escalation": map[string]any{
								"type":        "boolean",
								"description": "Controls whether a process can gain more privileges than its parent process.",
							},
							"privileged": map[string]any{
								"type":        "boolean",
								"description": "Run container in privileged mode, granting extensive host access.",
							},
							"capabilities": map[string]any{
								"type":        "object",
								"description": "POSIX capabilities added or dropped from the container process.",
								"properties": map[string]any{
									"drop": map[string]any{
										"type":        "array",
										"description": "List of Linux capabilities explicitly dropped from the container.",
										"items": map[string]any{
											"type": "string",
											"enum": []string{"ALL", "NET_RAW", "SYS_ADMIN", "NET_ADMIN"},
										},
									},
									"add": map[string]any{
										"type":        "array",
										"description": "List of Linux capabilities granted to the container.",
										"items": map[string]any{
											"type": "string",
										},
									},
								},
							},
						},
					},
					"resources": map[string]any{
						"type":        "object",
						"description": "Compute resource requests and limits required to prevent cluster node resource starvation.",
						"properties": map[string]any{
							"limits": map[string]any{
								"type": "object",
								"properties": map[string]any{
									"cpu":    map[string]any{"type": "string", "description": "Maximum CPU millicores allowed (e.g. '500m', '2')."},
									"memory": map[string]any{"type": "string", "description": "Maximum memory limit (e.g. '512Mi', '2Gi')."},
								},
							},
							"requests": map[string]any{
								"type": "object",
								"properties": map[string]any{
									"cpu":    map[string]any{"type": "string", "description": "Guaranteed CPU millicores requested."},
									"memory": map[string]any{"type": "string", "description": "Guaranteed memory requested."},
								},
							},
						},
					},
				},
				Required: []string{"kind", "manifest"},
			},
		},
	}
}
