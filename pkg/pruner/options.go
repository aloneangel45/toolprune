package pruner

import (
	"strings"
)

// PruneTier defines the optimization level applied to tool schemas.
type PruneTier string

// Tier is an alias to PruneTier for ergonomic compatibility.
type Tier = PruneTier

const (
	// TierNone represents raw pass-through without schema modification (0% reduction).
	TierNone PruneTier = "none"

	// TierLossless represents Tier 1 lossless structural minification (~12% reduction).
	TierLossless PruneTier = "lossless"

	// TierCompact represents Tier 2 smart description condensation and regex squeezing (~37% reduction).
	TierCompact PruneTier = "compact"

	// TierAggressive represents Tier 3 parameter description pruning and self-evident optimization (~47% to ~65% reduction).
	TierAggressive PruneTier = "aggressive"

	// TierDynamic represents Tier 4 catalog indexing & on-demand hydration (>80% reduction).
	TierDynamic PruneTier = "dynamic"
)

// MinifyOptions configures Tier 1 lossless structural minification.
type MinifyOptions struct {
	// StripSchemaKeyword removes "$schema" declarations from root and sub-schemas.
	StripSchemaKeyword bool `json:"stripSchemaKeyword"`

	// StripTitle removes "title" annotations from schemas and properties.
	StripTitle bool `json:"stripTitle"`

	// StripID removes "$id" and "id" identification URIs.
	StripID bool `json:"stripId"`

	// StripComments removes "$comment" and non-standard "comment" annotations.
	StripComments bool `json:"stripComments"`

	// StripExamples removes "examples" arrays and "example" fields.
	StripExamples bool `json:"stripExamples"`

	// StripAdditionalPropsFalse removes "additionalProperties: false" when boolean.
	// Preserves object schemas in additionalProperties (dictionaries).
	StripAdditionalPropsFalse bool `json:"stripAdditionalPropsFalse"`

	// CleanEmptyContainers removes empty "required": [] or empty definitions/properties maps.
	CleanEmptyContainers bool `json:"cleanEmptyContainers"`
}

// DefaultMinifyOptions returns standard production defaults for Tier 1.
func DefaultMinifyOptions() MinifyOptions {
	return MinifyOptions{
		StripSchemaKeyword:        true,
		StripTitle:                true,
		StripID:                   true,
		StripComments:             true,
		StripExamples:             true,
		StripAdditionalPropsFalse: true,
		CleanEmptyContainers:      true,
	}
}

// CondenseOptions configures Tier 2 smart description condensation and regex squeezing.
type CondenseOptions struct {
	// MaxToolDescLen is the target character limit for top-level tool descriptions (default: 120).
	MaxToolDescLen int `json:"maxToolDescLen"`

	// MaxParamDescLen is the target character limit for parameter descriptions (default: 80).
	MaxParamDescLen int `json:"maxParamDescLen"`

	// StripCodeBlocks removes markdown code blocks (```...```) from descriptions.
	StripCodeBlocks bool `json:"stripCodeBlocks"`

	// StripHTMLTags removes HTML markup (<tag>...</tag>) from descriptions.
	StripHTMLTags bool `json:"stripHtmlTags"`

	// StripTutorialLists removes bulleted and numbered tutorial instructions.
	StripTutorialLists bool `json:"stripTutorialLists"`

	// StripInstructionMeta removes meta-phrases (e.g., "IMPORTANT:", "Selection Process:").
	StripInstructionMeta bool `json:"stripInstructionMeta"`

	// ExtractFirstSentence limits descriptions to the first grammatical sentence before length truncation.
	ExtractFirstSentence bool `json:"extractFirstSentence"`

	// MaxRegexLength is the character threshold beyond which regular expressions are squeezed (default: 25).
	MaxRegexLength int `json:"maxRegexLength"`

	// NormalizeRegexToFormat converts recognized complex regexes (ISO-8601, UUID, URI, email) to standard format strings.
	NormalizeRegexToFormat bool `json:"normalizeRegexToFormat"`

	// StripVerboseRegex drops unrecognized regex patterns longer than MaxRegexLength.
	StripVerboseRegex bool `json:"stripVerboseRegex"`
}

// DefaultCondenseOptions returns standard production defaults for Tier 2.
func DefaultCondenseOptions() CondenseOptions {
	return CondenseOptions{
		MaxToolDescLen:         120,
		MaxParamDescLen:        80,
		StripCodeBlocks:        true,
		StripHTMLTags:          true,
		StripTutorialLists:     true,
		StripInstructionMeta:   true,
		ExtractFirstSentence:   true,
		MaxRegexLength:         25,
		NormalizeRegexToFormat: true,
		StripVerboseRegex:      true,
	}
}

// ParamsOptions defines the configuration parameters for Tier 3 pruning.
type ParamsOptions struct {
	// SelfEvidentParams holds the normalized dictionary of parameter names
	// whose descriptions should be stripped completely.
	SelfEvidentParams map[string]struct{} `json:"-"`

	// MaxNonSelfEvidentLen defines the max rune length for non-self-evident descriptions.
	MaxNonSelfEvidentLen int `json:"maxNonSelfEvidentLen"`

	// StripValidators specifies whether to strip decorative validators (minLength, etc.).
	StripValidators bool `json:"stripValidators"`

	// StripLongPatterns specifies whether to drop pattern regexes longer than PatternMaxLen.
	StripLongPatterns bool `json:"stripLongPatterns"`

	// PatternMaxLen is the threshold above which regex patterns are dropped.
	PatternMaxLen int `json:"patternMaxLen"`

	// Invariant flags (enforced in Tier 3)
	PreserveTypes    bool `json:"preserveTypes"`
	PreserveRequired bool `json:"preserveRequired"`
	PreserveEnums    bool `json:"preserveEnums"`
}

// DefaultParamsOptions returns the standard Tier 3 pruning configuration.
func DefaultParamsOptions() ParamsOptions {
	return ParamsOptions{
		SelfEvidentParams:    GetDefaultSelfEvidentDictionary(),
		MaxNonSelfEvidentLen: 50,
		StripValidators:      true,
		StripLongPatterns:    true,
		PatternMaxLen:        25,
		PreserveTypes:        true,
		PreserveRequired:     true,
		PreserveEnums:        true,
	}
}

// GetDefaultSelfEvidentDictionary initializes the set of self-evident parameters.
func GetDefaultSelfEvidentDictionary() map[string]struct{} {
	words := []string{
		// Identifiers & Core Attributes
		"id", "uuid", "name", "title", "key", "value", "type", "format",
		"status", "token", "author", "description", "summary",

		// Version Control & SCM
		"owner", "repo", "path", "branch", "sha", "ref", "commit",
		"commit_id", "pull_number", "issue_number", "tag",

		// Web & Filesystem
		"filepath", "filename", "directory", "dir", "uri", "url",
		"host", "hostname", "port",

		// Pagination & Sizing
		"page", "per_page", "page_size", "limit", "offset", "cursor",
		"next_cursor", "count", "size", "depth",

		// Search & Filtering
		"query", "q", "search", "filter", "sort", "order", "direction",

		// Content & Execution
		"content", "message", "body", "text", "sql", "command",
		"prompt", "comment", "data",
	}

	dict := make(map[string]struct{}, len(words))
	for _, w := range words {
		dict[strings.ToLower(w)] = struct{}{}
	}
	return dict
}

// PruneOptions encapsulates tier selection and detailed configuration.
type PruneOptions struct {
	Tier                   PruneTier       `json:"tier"`
	MaxSchemaTokens        int             `json:"maxSchemaTokens,omitempty"`
	MaxToolDescLength      int             `json:"maxToolDescLength"`
	MaxParamDescLength     int             `json:"maxParamDescLength"`
	StripRegexThresholdLen int             `json:"stripRegexThresholdLen"`
	StripSelfEvident       bool            `json:"stripSelfEvident"`
	StrictDraft07          bool            `json:"strictDraft07"`
	PreserveEnums          bool            `json:"preserveEnums"`
	PreserveRequired       bool            `json:"preserveRequired"`
	PreserveTypes          bool            `json:"preserveTypes"`
	Minify                 MinifyOptions   `json:"minify"`
	Condense               CondenseOptions `json:"condense"`
	Params                 ParamsOptions   `json:"params"`
}

// Options is an alias for PruneOptions for ergonomic compatibility.
type Options = PruneOptions

// DefaultPruneOptions returns production defaults for a given tier.
func DefaultPruneOptions(tier PruneTier) PruneOptions {
	if tier == "" {
		tier = TierAggressive
	}
	return PruneOptions{
		Tier:                   tier,
		MaxToolDescLength:      120,
		MaxParamDescLength:     80,
		StripRegexThresholdLen: 25,
		StripSelfEvident:       true,
		StrictDraft07:          true,
		PreserveEnums:          true,
		PreserveRequired:       true,
		PreserveTypes:          true,
		Minify:                 DefaultMinifyOptions(),
		Condense:               DefaultCondenseOptions(),
		Params:                 DefaultParamsOptions(),
	}
}

// DefaultOptions returns production defaults for a given tier (convenience wrapper).
func DefaultOptions(tier PruneTier) PruneOptions {
	return DefaultPruneOptions(tier)
}
