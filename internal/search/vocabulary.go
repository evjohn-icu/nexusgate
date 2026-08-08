package search

import (
	"sort"
	"strings"
)

// The controlled vocabulary is the offline bridge between raw query text and
// canonical constraint values. It is a superset of discovery's alias table
// (which stays untouched: the golden set pins its tokenization), covering the
// constraint families the compiler and the evidence gate share. One canonical
// value, one family: 汽车/轿车/car/vehicle all resolve to "car", so "car" in
// the query and "vehicle" in a shot's objects are the same constraint.
//
// Matching follows discovery's own discipline so the two layers cannot
// disagree: ASCII words match as whole words ("car" must never match
// "carefree", "carpet" — and "train" must never match "raining" — both are
// live retrieval findings), CJK words match as substrings because Chinese
// text has no word boundaries.

// CanonicalMatch is one canonical family hit inside a text, with the earliest
// surface position so the compiler can preserve query word order and apply
// positional negation.
type CanonicalMatch struct {
	Canonical string
	Type      ConstraintType
	Position  int
}

// canonicalFamily is one vocabulary entry: a canonical value, its constraint
// type, and every surface word that resolves to it.
type canonicalFamily struct {
	canonical string
	typ       ConstraintType
	words     []string
}

// vocabulary is the full controlled vocabulary.
var vocabulary = []canonicalFamily{
	// objects
	{canonical: "car", typ: ConstraintObject, words: []string{"汽车", "轿车", "车", "车流", "交通", "car", "cars", "vehicle", "vehicles", "sedan", "truck", "卡车", "traffic"}},
	{canonical: "umbrella", typ: ConstraintObject, words: []string{"伞", "雨伞", "umbrella", "umbrellas"}},
	{canonical: "person", typ: ConstraintObject, words: []string{"人", "行人", "人们", "person", "people", "human", "pedestrian"}},
	{canonical: "dog", typ: ConstraintObject, words: []string{"狗", "dog", "dogs"}},
	{canonical: "cat", typ: ConstraintObject, words: []string{"猫", "cat", "cats"}},
	{canonical: "boat", typ: ConstraintObject, words: []string{"船", "boat", "boats", "ship"}},
	{canonical: "laptop", typ: ConstraintObject, words: []string{"笔记本", "laptop", "computer"}},
	{canonical: "train", typ: ConstraintObject, words: []string{"火车", "列车", "train", "trains"}},
	{canonical: "bridge", typ: ConstraintObject, words: []string{"桥", "bridge", "bridges"}},
	{canonical: "bicycle", typ: ConstraintObject, words: []string{"自行车", "单车", "bicycle", "bike", "bikes"}},
	{canonical: "flower", typ: ConstraintObject, words: []string{"花", "flower", "flowers", "tulip", "tulips", "郁金香"}},
	{canonical: "cup", typ: ConstraintObject, words: []string{"杯", "杯子", "cup", "coffee cup"}},
	{canonical: "table", typ: ConstraintObject, words: []string{"桌", "桌子", "table", "desk"}},
	{canonical: "ocean", typ: ConstraintObject, words: []string{"海", "大海", "ocean", "sea"}},
	{canonical: "sun", typ: ConstraintObject, words: []string{"太阳", "sun"}},

	// actions
	{canonical: "walking", typ: ConstraintAction, words: []string{"走", "行走", "步行", "漫步", "walk", "walking", "stroll", "strolling"}},
	{canonical: "running", typ: ConstraintAction, words: []string{"跑", "奔跑", "run", "running"}},
	{canonical: "driving", typ: ConstraintAction, words: []string{"开", "驾驶", "drive", "driving"}},
	{canonical: "eating", typ: ConstraintAction, words: []string{"吃", "eating", "eat"}},
	{canonical: "drinking", typ: ConstraintAction, words: []string{"喝", "drink", "drinking"}},
	{canonical: "sitting", typ: ConstraintAction, words: []string{"坐", "sit", "sitting"}},

	// scenes
	{canonical: "street", typ: ConstraintScene, words: []string{"街", "街道", "路", "马路", "street", "streets", "road", "alley", "avenue"}},
	{canonical: "city", typ: ConstraintScene, words: []string{"城市", "城", "city", "cities", "urban", "downtown"}},
	{canonical: "beach", typ: ConstraintScene, words: []string{"海边", "沙滩", "海滩", "beach", "beaches"}},
	{canonical: "park", typ: ConstraintScene, words: []string{"公园", "park", "parks"}},
	{canonical: "market", typ: ConstraintScene, words: []string{"市场", "集市", "market", "markets"}},
	{canonical: "kitchen", typ: ConstraintScene, words: []string{"厨房", "kitchen"}},
	{canonical: "cafe", typ: ConstraintScene, words: []string{"咖啡厅", "咖啡馆", "cafe", "coffee shop"}},
	{canonical: "office", typ: ConstraintScene, words: []string{"办公室", "office"}},
	{canonical: "mountain", typ: ConstraintScene, words: []string{"山", "山间", "mountain", "mountains"}},
	{canonical: "river", typ: ConstraintScene, words: []string{"河", "河流", "river"}},
	{canonical: "station", typ: ConstraintScene, words: []string{"车站", "火车站", "station", "platform", "站台"}},
	{canonical: "school", typ: ConstraintScene, words: []string{"学校", "校园", "school", "schoolyard"}},
	{canonical: "library", typ: ConstraintScene, words: []string{"图书馆", "library"}},
	{canonical: "restaurant", typ: ConstraintScene, words: []string{"餐厅", "饭馆", "restaurant", "dining"}},
	{canonical: "stadium", typ: ConstraintScene, words: []string{"体育场", "场馆", "stadium", "arena"}},
	{canonical: "factory", typ: ConstraintScene, words: []string{"工厂", "factory"}},
	{canonical: "garden", typ: ConstraintScene, words: []string{"花园", "园", "garden", "gardens"}},

	// weather
	{canonical: "rain", typ: ConstraintWeather, words: []string{"雨", "下雨", "雨天", "rain", "rainy", "raining"}},
	{canonical: "snow", typ: ConstraintWeather, words: []string{"雪", "下雪", "snow", "snowy", "snowing"}},
	{canonical: "fog", typ: ConstraintWeather, words: []string{"雾", "fog", "foggy", "mist"}},
	{canonical: "wind", typ: ConstraintWeather, words: []string{"风", "wind", "windy"}},
	{canonical: "sunny", typ: ConstraintWeather, words: []string{"晴", "晴天", "sunny", "clear"}},

	// time
	{canonical: "night", typ: ConstraintTime, words: []string{"夜晚", "夜里", "晚上", "夜", "晚", "night", "nighttime", "evening"}},
	{canonical: "day", typ: ConstraintTime, words: []string{"白天", "日", "day", "daytime"}},
	{canonical: "dawn", typ: ConstraintTime, words: []string{"黎明", "拂晓", "清晨", "dawn"}},
	{canonical: "dusk", typ: ConstraintTime, words: []string{"黄昏", "傍晚", "dusk"}},
	{canonical: "morning", typ: ConstraintTime, words: []string{"早晨", "早上", "morning"}},

	// mood
	{canonical: "lonely", typ: ConstraintMood, words: []string{"孤独", "lonely"}},
	{canonical: "depressing", typ: ConstraintMood, words: []string{"压抑", "阴郁", "depressing", "oppressive"}},
	{canonical: "cinematic", typ: ConstraintMood, words: []string{"电影感", "氛围", "cinematic", "moody"}},

	// camera / shot size
	{canonical: "aerial", typ: ConstraintCamera, words: []string{"航拍", "俯瞰", "aerial", "bird's eye"}},
	{canonical: "close_up", typ: ConstraintShotSize, words: []string{"特写", "close-up", "closeup", "close up"}},
	{canonical: "wide", typ: ConstraintShotSize, words: []string{"全景", "广角", "wide", "establishing"}},
	{canonical: "medium", typ: ConstraintShotSize, words: []string{"中景", "medium shot", "medium"}},
}

// allVocabularyWords indexes every surface word to its family for the
// pass-through exclusion and position lookups.
var allVocabularyWords = func() map[string]string {
	m := make(map[string]string)
	for _, family := range vocabulary {
		for _, word := range family.words {
			if _, exists := m[word]; !exists {
				m[word] = family.canonical
			}
		}
	}
	return m
}()

// Canonicalize returns every canonical family a text resolves to, sorted by
// earliest surface position (query word order). Raw terms absent from the
// vocabulary pass through as their own canonical value (objects by default)
// so vocabulary outside the table keeps working. CJK runs longer than three
// characters are sentences, not terms, and are skipped — their covered
// substrings are already canonicalized, and leaking the whole run would
// fabricate a constraint nothing can satisfy.
func Canonicalize(text string) []CanonicalMatch {
	text = strings.ToLower(strings.TrimSpace(text))
	if text == "" {
		return nil
	}
	type found struct {
		canonical string
		typ       ConstraintType
		position  int
	}
	var matches []found
	seen := map[string]bool{}
	appendMatch := func(canonical string, typ ConstraintType, position int) {
		if canonical == "" || seen[canonical] {
			return
		}
		seen[canonical] = true
		matches = append(matches, found{canonical: canonical, typ: typ, position: position})
	}
	asciiTokens := asciiWordSet(text)
	for _, family := range vocabulary {
		position := -1
		for _, word := range family.words {
			if hasCJK(word) {
				if idx := strings.Index(text, word); idx >= 0 {
					position = idx
					break
				}
				continue
			}
			if asciiTokens[word] {
				position = wordPosition(text, word)
				break
			}
		}
		if position >= 0 {
			appendMatch(family.canonical, family.typ, position)
		}
	}
	for _, token := range strings.FieldsFunc(text, func(r rune) bool {
		return !isTermRune(r)
	}) {
		if token == "" || seen[token] {
			continue
		}
		if hasCJK(token) {
			length := len([]rune(token))
			if length < 2 || length > 3 {
				continue
			}
			if isCJKStopword(token) {
				continue
			}
			if containsAny(token, allVocabularyWords) || containsAny(token, negationWordStrings) {
				continue
			}
		} else {
			if len([]rune(token)) < 3 {
				continue
			}
			if _, claimed := allVocabularyWords[token]; claimed {
				continue
			}
			if _, isNegation := negationWordStrings[token]; isNegation {
				continue
			}
		}
		appendMatch(token, ConstraintObject, strings.Index(text, token))
	}
	sort.SliceStable(matches, func(i, j int) bool { return matches[i].position < matches[j].position })
	out := make([]CanonicalMatch, 0, len(matches))
	for _, m := range matches {
		out = append(out, CanonicalMatch{Canonical: m.canonical, Type: m.typ, Position: m.position})
	}
	return out
}

// CanonicalValues returns just the canonical strings of Canonicalize.
func CanonicalValues(text string) []string {
	matches := Canonicalize(text)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, m.Canonical)
	}
	return out
}

// asciiWordSet builds the whole-word set of a text's ASCII tokens — the same
// boundary discovery uses, so "car" matches "car" but not "carefree".
func asciiWordSet(text string) map[string]bool {
	set := map[string]bool{}
	for _, field := range strings.FieldsFunc(text, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	}) {
		set[field] = true
	}
	return set
}

// wordPosition returns the byte offset of the whole word inside text.
func wordPosition(text, word string) int {
	lower := strings.ToLower(text)
	for i := 0; i+len(word) <= len(lower); i++ {
		if lower[i:i+len(word)] != word {
			continue
		}
		beforeOK := i == 0 || !isWordByte(lower[i-1])
		afterOK := i+len(word) == len(lower) || !isWordByte(lower[i+len(word)])
		if beforeOK && afterOK {
			return i
		}
	}
	return -1
}

func isWordByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= '0' && b <= '9'
}

func containsAny(value string, words map[string]string) bool {
	for word := range words {
		if strings.Contains(value, word) {
			return true
		}
	}
	return false
}

// hasCJK reports whether the string carries CJK runes. Alias words with CJK
// runes are matched as substrings; everything else as a whole token.
func hasCJK(s string) bool {
	for _, r := range s {
		if r >= 0x2E80 {
			return true
		}
	}
	return false
}

func isTermRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r >= '\u3040' && r <= '\u9fff'
}

// negationWordClass marks how a negation word negates. Direct negations (没有,
// 无人, no, without, ...) negate any constraint whose surface word falls in
// their window — "没有下雨" negates rain. Absence descriptors (空, empty,
// abandoned, ...) describe a scene as lacking things and only negate
// object-family constraints — "empty beach" does not negate beach itself.
type negationWordClass int

const (
	negationDirect negationWordClass = iota
	negationAbsentDescriptor
)

type negationWord struct {
	word  string
	class negationWordClass
}

var negationWords = []negationWord{
	{word: "没有", class: negationDirect},
	{word: "无人", class: negationDirect},
	{word: "无", class: negationDirect},
	{word: "不见", class: negationDirect},
	{word: "no", class: negationDirect},
	{word: "without", class: negationDirect},
	{word: "空", class: negationAbsentDescriptor},
	{word: "empty", class: negationAbsentDescriptor},
	{word: "abandoned", class: negationAbsentDescriptor},
	{word: "unoccupied", class: negationAbsentDescriptor},
}

var negationWordStrings = func() map[string]string {
	m := make(map[string]string)
	for _, n := range negationWords {
		m[n.word] = n.word
	}
	return m
}()

// cjkStopwords are generic CJK nouns that would otherwise pass through as
// object constraints ("地方"/"场景" become musts nothing can confirm, and the
// fact gate then drops every shot). They carry no retrieval meaning.
var cjkStopwords = []string{"地方", "场景", "画面", "镜头", "素材", "东西", "内容", "片段"}

func isCJKStopword(token string) bool {
	for _, stop := range cjkStopwords {
		if stop == token {
			return true
		}
	}
	return false
}

// NegationSpan is a negation word occurrence: byte start/end, its class and
// whether it is a CJK word (byte arithmetic differs: a CJK rune is 3 bytes).
type NegationSpan struct {
	Start int
	End   int
	Class negationWordClass
	CJK   bool
}

// NegationSpans finds every negation word occurrence in the text (CJK as
// substring, ASCII as whole word). The compiler uses these for positional
// negation; the evidence gate uses HasNegation for the coarse check.
func NegationSpans(text string) []NegationSpan {
	text = strings.ToLower(strings.TrimSpace(text))
	if text == "" {
		return nil
	}
	ascii := asciiWordSet(text)
	var out []NegationSpan
	for _, n := range negationWords {
		cjk := hasCJK(n.word)
		if cjk {
			idx := strings.Index(text, n.word)
			for idx >= 0 {
				out = append(out, NegationSpan{Start: idx, End: idx + len(n.word), Class: n.class, CJK: true})
				next := strings.Index(text[idx+len(n.word):], n.word)
				if next < 0 {
					break
				}
				idx += len(n.word) + next
			}
			continue
		}
		if !ascii[n.word] {
			continue
		}
		pos := wordPosition(text, n.word)
		if pos >= 0 {
			out = append(out, NegationSpan{Start: pos, End: pos + len(n.word), Class: n.class})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Start < out[j].Start })
	return out
}

// HasNegation reports whether the text carries any explicit absence marker.
// It is deliberately coarse — the compiler's positional logic is the precise
// path; this is the fast guard shared with the evidence gate.
func HasNegation(text string) bool {
	return len(NegationSpans(text)) > 0
}

// Negates reports whether a canonical constraint at the given surface byte
// position is negated by a negation span. The window covers the negation word
// plus one following rune (3 bytes for CJK, 2 for ASCII — enough for the
// space in "no person" but not far enough to catch a scene word after a
// separator like 的). Absence descriptors (empty/abandoned/空) only negate
// object-family constraints, so "empty beach" keeps beach positive while
// "empty street" still negates nothing the compiler put in the window.
func Negates(span NegationSpan, constraintType ConstraintType, position int) bool {
	window := span.End + 2
	if span.CJK {
		window = span.End + 3
	}
	if position < span.Start || position >= window {
		return false
	}
	if span.Class == negationAbsentDescriptor && constraintType != ConstraintObject {
		return false
	}
	return true
}
