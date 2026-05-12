// Package chunker splits source files into semantically coherent chunks using
// tree-sitter ASTs.
//
// The cAST algorithm recursively traverses a file's AST. Nodes whose byte
// span exceeds [Config.MaxChunkSize] are split; adjacent small nodes are
// merged until [Config.MinChunkSize] is reached. The result is a []Chunk
// where each element is a coherent source unit ready for embedding.
//
// [Chunker] is constructed once via [NewChunker] and is safe to reuse across
// files. Language configurations are injected at construction time; see the
// langs package for the full set of built-in languages.
package chunker

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	sitter "github.com/tree-sitter/go-tree-sitter"
)

var (
	// ErrUnsupportedLanguage is returned by ChunkFile when no LanguageConfig
	// matches the file's extension.
	ErrUnsupportedLanguage = errors.New("unsupported language")

	// ErrParseFailed is returned by ChunkFile when tree-sitter cannot produce
	// a valid syntax tree for the given source.
	ErrParseFailed = errors.New("tree-sitter parse failed")
)

// Config controls the cAST algorithm size thresholds (in bytes).
type Config struct {
	MaxChunkSize int // split when a node exceeds this; default 1500
	MinChunkSize int // merge siblings until this is reached; default 50
}

// kindSets holds pre-computed O(1) lookup maps for a language's top-level and
// nested node kinds, replacing the linear-scan isKindIn calls.
type kindSets struct {
	topLevel map[string]struct{}
	nested   map[string]struct{}
}

// DefaultConfig returns production-tuned defaults (~500 tokens at ~3 chars/token).
func DefaultConfig() Config {
	return Config{MaxChunkSize: 1500, MinChunkSize: 50}
}

// Chunker parses and chunks source files. Construct once via NewChunker, reuse across files.
type Chunker struct {
	cfg     Config
	byExt   map[string]LanguageConfig
	kindMap map[string]kindSets // language name -> pre-computed kind lookup sets
}

// NewChunker constructs a Chunker from the provided language configs and size
// thresholds. Returns an error if langs is empty, any Grammar pointer is nil,
// or two configs claim the same file extension.
// The returned Chunker is safe to reuse across files and goroutines.
func NewChunker(langs []LanguageConfig, cfg Config) (*Chunker, error) {
	if len(langs) == 0 {
		return nil, errors.New("langs must not be empty")
	}
	if cfg.MaxChunkSize <= 0 {
		return nil, errors.New("MaxChunkSize must be positive")
	}
	if cfg.MinChunkSize <= 0 {
		return nil, errors.New("MinChunkSize must be positive")
	}
	byExt := make(map[string]LanguageConfig, len(langs))
	for _, lang := range langs {
		if lang.Grammar == nil {
			return nil, fmt.Errorf("language %q has a nil Grammar pointer", lang.Name)
		}
		for _, ext := range lang.Extensions {
			key := strings.ToLower(ext)
			if existing, ok := byExt[key]; ok {
				return nil, fmt.Errorf("extension %q claimed by both %q and %q", key, existing.Name, lang.Name)
			}
			byExt[key] = lang
		}
	}
	// Pre-compute O(1) kind lookup sets for each language, also covering
	// injection grammars so that injected languages get the same benefit.
	km := make(map[string]kindSets, len(langs))
	var buildKindSets func(lang LanguageConfig)
	buildKindSets = func(lang LanguageConfig) {
		if _, ok := km[lang.Name]; ok {
			return
		}
		tl := make(map[string]struct{}, len(lang.NodeKinds.TopLevel))
		for _, k := range lang.NodeKinds.TopLevel {
			tl[k] = struct{}{}
		}
		n := make(map[string]struct{}, len(lang.NodeKinds.Nested))
		for _, k := range lang.NodeKinds.Nested {
			n[k] = struct{}{}
		}
		km[lang.Name] = kindSets{topLevel: tl, nested: n}
		for _, inj := range lang.Injections {
			for _, injLang := range inj.Grammars {
				buildKindSets(injLang)
			}
		}
	}
	for _, lang := range langs {
		buildKindSets(lang)
	}

	return &Chunker{cfg: cfg, byExt: byExt, kindMap: km}, nil
}

// atom is an internal unit collected during the recursive split phase.
type atom struct {
	startByte  int
	endByte    int
	startLine  int // 1-based
	endLine    int // 1-based
	startCol   int // 0-based
	endCol     int // 0-based
	kind       string
	name       string // eagerly resolved from NameField; safe to use after tree is closed
	parent     string // eagerly resolved parent name; safe to use after tree is closed
	isTopLevel bool   // true when this atom is a top-level boundary (not nested inside another top-level node)
	// src is the full source text of the file this atom came from.
	// All atoms in a merge group must share the same src backing array.
	// The injection system must pass the original full-file src, never a sub-slice.
	src      []byte
	filePath string
	langCfg  *LanguageConfig
}

// hasTopLevelAncestor reports whether node has an ancestor whose kind is in the
// topLevel set. Used to distinguish truly top-level nodes from nested ones when
// a kind appears in both TopLevel and Nested (e.g. Python's function_definition).
func (c *Chunker) hasTopLevelAncestor(node *sitter.Node, topLevel map[string]struct{}) bool {
	for p := node.Parent(); p != nil; p = p.Parent() {
		if _, ok := topLevel[p.Kind()]; ok {
			return true
		}
	}
	return false
}

// ChunkFile parses src as the language inferred from filePath's extension
// and returns semantically coherent chunks ready for embedding.
// Returns [ErrUnsupportedLanguage] when no LanguageConfig matches the
// extension, and [ErrParseFailed] when tree-sitter cannot parse the source.
// Empty src returns nil, nil.
func (c *Chunker) ChunkFile(filePath string, src []byte) ([]Chunk, error) {
	if len(src) == 0 {
		return nil, nil
	}
	ext := strings.ToLower(filepath.Ext(filePath))
	langCfg, ok := c.byExt[ext]
	if !ok {
		return nil, ErrUnsupportedLanguage
	}

	// Parser is created per call rather than pooled. sync.Pool evicts on GC,
	// permanently leaking the CGo-backed C memory (~100ns overhead per file).
	parser := sitter.NewParser()
	defer parser.Close()

	if err := parser.SetLanguage(langCfg.Grammar); err != nil {
		return nil, fmt.Errorf("%w: setting language: %v", ErrParseFailed, err)
	}

	tree := parser.Parse(src, nil)
	if tree == nil {
		return nil, ErrParseFailed
	}
	defer tree.Close()

	root := tree.RootNode()
	atoms := make([]atom, 0, max(len(src)/c.cfg.MinChunkSize, 8))
	// langCfg is a stack-local copy; &langCfg is valid only within this call since atoms never outlive ChunkFile.
	c.collectAtoms(root, src, &langCfg, filePath, &atoms)

	if len(langCfg.Injections) > 0 {
		atoms = c.applyInjections(atoms, root, src, &langCfg, filePath)
	}

	return c.mergeAtoms(atoms), nil
}

// collectAtoms recursively collects semantic atoms from the AST using the cAST algorithm.
func (c *Chunker) collectAtoms(node *sitter.Node, src []byte, langCfg *LanguageConfig, filePath string, atoms *[]atom) {
	if node == nil {
		return
	}

	kind := node.Kind()
	if node.EndByte() < node.StartByte() {
		return
	}
	byteSpan := int(node.EndByte() - node.StartByte())

	sets := c.kindMap[langCfg.Name]
	_, inTopLevel := sets.topLevel[kind]
	_, inNested := sets.nested[kind]
	isSemanticBoundary := inTopLevel || inNested

	if isSemanticBoundary && byteSpan <= c.cfg.MaxChunkSize {
		sp := node.StartPosition()
		ep := node.EndPosition()

		// Determine whether this atom is truly top-level. When a kind appears
		// in both TopLevel and Nested (e.g. Python's function_definition for
		// module-level functions vs class methods), check the AST parent chain.
		atomIsTopLevel := inTopLevel
		if inTopLevel && inNested {
			atomIsTopLevel = !c.hasTopLevelAncestor(node, sets.topLevel)
		}

		// Resolve name and parent eagerly while the parse tree is still alive.
		// This is critical for injected grammars whose trees are closed immediately
		// after collectAtoms returns — storing the *Node would leave a dangling pointer.
		name := ""
		if langCfg.NodeKinds.NameField != "" {
			if nameNode := node.ChildByFieldName(langCfg.NodeKinds.NameField); nameNode != nil {
				name = nameNode.Utf8Text(src)
			}
		}
		parent := ""
		if inNested {
			parent = c.extractParentName(node, src, langCfg)
		}

		*atoms = append(*atoms, atom{
			startByte:  int(node.StartByte()),
			endByte:    int(node.EndByte()),
			startLine:  int(sp.Row) + 1,
			endLine:    int(ep.Row) + 1,
			startCol:   int(sp.Column),
			endCol:     int(ep.Column),
			kind:       kind,
			name:       name,
			parent:     parent,
			isTopLevel: atomIsTopLevel,
			src:        src,
			filePath:   filePath,
			langCfg:    langCfg,
		})
		return
	}

	namedCount := node.NamedChildCount()

	if byteSpan > c.cfg.MaxChunkSize {
		if namedCount == 0 {
			c.lineSplitAtom(node, src, langCfg, filePath, atoms)
			return
		}
		for i := range namedCount {
			c.collectAtoms(node.NamedChild(i), src, langCfg, filePath, atoms)
		}
		return
	}

	// Small non-semantic node — traverse children transparently.
	for i := range namedCount {
		c.collectAtoms(node.NamedChild(i), src, langCfg, filePath, atoms)
	}
}

// lineSplitAtom splits an oversized leaf node into line-based atoms.
// When a single line itself exceeds MaxChunkSize, it is further split by bytes.
// Note: Start.Column and End.Column are always 0 for line-split atoms — column
// tracking is not meaningful for minified or generated content that triggers this path.
func (c *Chunker) lineSplitAtom(node *sitter.Node, src []byte, langCfg *LanguageConfig, filePath string, atoms *[]atom) {
	nodeBytes := src[node.StartByte():node.EndByte()]
	baseOffset := int(node.StartByte())
	baseRow := int(node.StartPosition().Row)

	lines := bytes.SplitAfter(nodeBytes, []byte("\n"))

	chunkStart := 0
	chunkStartRow := baseRow
	lineOffset := 0

	flush := func(end int, endRow int) {
		if end <= chunkStart {
			return
		}
		*atoms = append(*atoms, atom{
			startByte: baseOffset + chunkStart,
			endByte:   baseOffset + end,
			startLine: chunkStartRow + 1,
			endLine:   endRow + 1,
			kind:      "line",
			src:       src,
			filePath:  filePath,
			langCfg:   langCfg,
		})
		chunkStart = end
		chunkStartRow = endRow
	}

	for _, line := range lines {
		lineEnd := lineOffset + len(line)

		// If adding this line would exceed MaxChunkSize and we have accumulated
		// content, flush the accumulated content first.
		if lineEnd-chunkStart > c.cfg.MaxChunkSize && lineOffset > chunkStart {
			endRow := baseRow + bytes.Count(nodeBytes[chunkStart:lineOffset], []byte("\n"))
			flush(lineOffset, endRow)
		}

		// If a single line itself exceeds MaxChunkSize, split it by bytes.
		for lineEnd-chunkStart > c.cfg.MaxChunkSize {
			splitAt := chunkStart + c.cfg.MaxChunkSize
			endRow := baseRow + bytes.Count(nodeBytes[chunkStart:splitAt], []byte("\n"))
			flush(splitAt, endRow)
		}

		lineOffset = lineEnd
	}

	if chunkStart < len(nodeBytes) {
		endRow := baseRow + bytes.Count(nodeBytes[chunkStart:], []byte("\n"))
		flush(len(nodeBytes), endRow)
	}
}

// mergeAtoms greedily merges adjacent atoms into chunks respecting MaxChunkSize.
// Adjacent atoms are merged until adding the next would exceed MaxChunkSize.
// A top-level semantic boundary triggers a split only when the accumulated group
// has already reached MinChunkSize, preventing hairline chunks from top-level nodes.
func (c *Chunker) mergeAtoms(atoms []atom) []Chunk {
	if len(atoms) == 0 {
		return nil
	}

	chunks := make([]Chunk, 0, max(len(atoms)/2, 4))
	groupStart := 0

	for i := 1; i < len(atoms); i++ {
		accumulated := atoms[i-1].endByte - atoms[groupStart].startByte
		nextIsTopLevel := atoms[i].isTopLevel

		exceedsMax := (atoms[i].endByte - atoms[groupStart].startByte) > c.cfg.MaxChunkSize
		splitOnBoundary := nextIsTopLevel && accumulated >= c.cfg.MinChunkSize
		langChanged := atoms[i].langCfg.Name != atoms[groupStart].langCfg.Name

		if exceedsMax || splitOnBoundary || langChanged {
			chunks = append(chunks, c.buildChunk(atoms[groupStart:i]))
			groupStart = i
		}
	}
	chunks = append(chunks, c.buildChunk(atoms[groupStart:]))

	return chunks
}

// buildChunk converts a slice of atoms into a Chunk with full metadata.
func (c *Chunker) buildChunk(atoms []atom) Chunk {
	if len(atoms) == 0 {
		panic("buildChunk called with empty atoms")
	}
	first := atoms[0]
	last := atoms[len(atoms)-1]

	content := string(first.src[first.startByte:last.endByte])

	// name and parent are pre-resolved in collectAtoms while the owning parse tree
	// was still alive, so we can safely use them here even for injected grammars.
	return Chunk{
		Content:  content,
		FilePath: first.filePath,
		Language: first.langCfg.Name,
		NodeKind: first.kind,
		Name:     first.name,
		Parent:   first.parent,
		Start:    Position{Line: first.startLine, Column: first.startCol, ByteOffset: first.startByte},
		End:      Position{Line: last.endLine, Column: last.endCol, ByteOffset: last.endByte},
	}
}

// extractParentName walks up the AST to find the nearest top-level ancestor's name.
func (c *Chunker) extractParentName(node *sitter.Node, src []byte, langCfg *LanguageConfig) string {
	sets := c.kindMap[langCfg.Name]
	p := node.Parent()
	for p != nil {
		if _, ok := sets.topLevel[p.Kind()]; ok {
			if langCfg.NodeKinds.NameField != "" {
				if nameNode := p.ChildByFieldName(langCfg.NodeKinds.NameField); nameNode != nil {
					return nameNode.Utf8Text(src)
				}
			}
			return ""
		}
		p = p.Parent()
	}
	return ""
}

// applyInjections runs each configured InjectionRule as a post-pass over atoms,
// replacing section-level atoms with semantically richer atoms produced by
// re-parsing the embedded content with a secondary grammar.
func (c *Chunker) applyInjections(atoms []atom, root *sitter.Node, src []byte, langCfg *LanguageConfig, filePath string) []atom {
	for _, rule := range langCfg.Injections {
		atoms = c.applyInjection(atoms, root, src, rule, filePath)
	}
	return atoms
}

// applyInjection applies a single InjectionRule to all matching container nodes in the AST.
func (c *Chunker) applyInjection(atoms []atom, root *sitter.Node, src []byte, rule InjectionRule, filePath string) []atom {
	var containers []*sitter.Node
	collectNodesByKind(root, rule.ContainerKind, &containers)
	for _, container := range containers {
		atoms = c.injectContainer(atoms, container, src, rule, filePath)
	}
	return atoms
}

// injectContainer re-parses the embedded content of one container node with a secondary grammar,
// replacing its atoms with semantically richer atoms. Returns atoms unchanged on any error or
// when the content is empty — injection is best-effort and degrades to section-level atoms.
func (c *Chunker) injectContainer(atoms []atom, container *sitter.Node, src []byte, rule InjectionRule, filePath string) []atom {
	rawText := findChildByKind(container, rule.ContentKind)
	if rawText == nil || rawText.StartByte() == rawText.EndByte() {
		return atoms
	}

	langName := rule.DefaultLang
	if rule.LangAttr != "" {
		if startTag := findStartTag(container); startTag != nil {
			if v := attrValue(startTag, src, rule.LangAttr); v != "" {
				langName = v
			}
		}
	}

	injCfg, ok := rule.Grammars[langName]
	if !ok {
		return atoms
	}

	injParser := sitter.NewParser()
	defer injParser.Close()
	if err := injParser.SetLanguage(injCfg.Grammar); err != nil {
		return atoms
	}
	if err := injParser.SetIncludedRanges([]sitter.Range{{
		StartByte:  rawText.StartByte(),
		EndByte:    rawText.EndByte(),
		StartPoint: rawText.StartPosition(),
		EndPoint:   rawText.EndPosition(),
	}}); err != nil {
		return atoms
	}
	injTree := injParser.Parse(src, nil)
	if injTree == nil {
		return atoms
	}
	defer injTree.Close()

	var injAtoms []atom
	c.collectAtoms(injTree.RootNode(), src, &injCfg, filePath, &injAtoms)
	if len(injAtoms) == 0 {
		return atoms
	}

	return replaceAtomsInRange(atoms, int(container.StartByte()), int(container.EndByte()), injAtoms)
}

// collectNodesByKind does a depth-first walk collecting all nodes with the given kind.
// It does not recurse into matched nodes.
func collectNodesByKind(node *sitter.Node, kind string, out *[]*sitter.Node) {
	if node == nil {
		return
	}
	if node.Kind() == kind {
		*out = append(*out, node)
		return
	}
	for i := range node.NamedChildCount() {
		collectNodesByKind(node.NamedChild(i), kind, out)
	}
}

// findChildByKind returns the first child (named or anonymous) with the given kind, or nil.
func findChildByKind(node *sitter.Node, kind string) *sitter.Node {
	for i := range node.ChildCount() {
		if child := node.Child(i); child.Kind() == kind {
			return child
		}
	}
	return nil
}

// findStartTag returns the start_tag child of a container node, accounting for
// Vue grammar aliases (script_start_tag, style_start_tag, template_start_tag are
// aliased to start_tag; node.Kind() may return either form).
func findStartTag(node *sitter.Node) *sitter.Node {
	for i := range node.ChildCount() {
		child := node.Child(i)
		switch child.Kind() {
		case "start_tag", "script_start_tag", "style_start_tag", "template_start_tag":
			return child
		}
	}
	return nil
}

// attrValue walks a start_tag's attribute children looking for an attribute whose
// attribute_name equals attrName and returns its attribute_value text.
// Returns "" if the attribute is absent or has no value (e.g. boolean attributes like "setup").
func attrValue(startTag *sitter.Node, src []byte, attrName string) string {
	for i := range startTag.NamedChildCount() {
		attr := startTag.NamedChild(i)
		if attr.Kind() != "attribute" {
			continue
		}
		nameNode := findChildByKind(attr, "attribute_name")
		if nameNode == nil || nameNode.Utf8Text(src) != attrName {
			continue
		}
		// lang="ts" → quoted_attribute_value → attribute_value
		if quoted := findChildByKind(attr, "quoted_attribute_value"); quoted != nil {
			if val := findChildByKind(quoted, "attribute_value"); val != nil {
				return val.Utf8Text(src)
			}
		}
		if val := findChildByKind(attr, "attribute_value"); val != nil {
			return val.Utf8Text(src)
		}
	}
	return ""
}

// replaceAtomsInRange replaces all atoms whose byte range falls within [start, end]
// with replacements. If no atoms fall in the range, returns atoms unchanged
// without allocating a new slice.
func replaceAtomsInRange(atoms []atom, start, end int, replacements []atom) []atom {
	hasMatch := false
	for _, a := range atoms {
		if a.startByte >= start && a.endByte <= end {
			hasMatch = true
			break
		}
	}
	if !hasMatch {
		return atoms
	}

	result := make([]atom, 0, len(atoms)+len(replacements))
	inserted := false
	for _, a := range atoms {
		if a.startByte >= start && a.endByte <= end {
			if !inserted {
				result = append(result, replacements...)
				inserted = true
			}
			continue
		}
		result = append(result, a)
	}
	return result
}
