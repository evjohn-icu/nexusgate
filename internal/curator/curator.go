package curator

import (
	"sort"
	"strings"
	"unicode"

	"github.com/evjohn-icu/nexusslate/internal/domain"
)

// BuildProposals is intentionally bounded: it only proposes deterministic,
// reversible alias/canonical operations. It never emits SQL or mutates storage.
func BuildProposals(items []domain.UnresolvedTag, existing []domain.CanonicalTag) []domain.TagProposal {
	byCanonical := make(map[string]domain.CanonicalTag, len(existing))
	for _, tag := range existing {
		byCanonical[Normalize(tag.CanonicalName)] = tag
	}
	groups := map[string][]domain.UnresolvedTag{}
	for _, item := range items {
		key := semanticKey(item.NormalizedTag)
		groups[key] = append(groups[key], item)
	}
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var proposals []domain.TagProposal
	for _, key := range keys {
		group := groups[key]
		if len(group) == 0 {
			continue
		}
		canonical := chooseCanonical(group)
		aliases := make([]string, 0, len(group))
		affected := 0
		for _, item := range group {
			affected += item.AssetCount
			if item.NormalizedTag != canonical {
				aliases = append(aliases, item.NormalizedTag)
			}
		}
		if existingTag, ok := byCanonical[canonical]; ok {
			if len(aliases) == 0 {
				continue
			}
			proposals = append(proposals, domain.TagProposal{
				State: "pending", ProposalType: "add_aliases", CanonicalName: existingTag.CanonicalName,
				Payload:    map[string]any{"canonical_tag_id": existingTag.ID, "aliases": aliases},
				Confidence: confidenceFor(group), Reason: "格式归一化或高相似标签可映射到现有标准标签", AffectedAssets: affected,
			})
			continue
		}
		proposalType := "create_canonical"
		if len(aliases) > 0 {
			proposalType = "create_canonical_with_aliases"
		}
		proposals = append(proposals, domain.TagProposal{
			State: "pending", ProposalType: proposalType, CanonicalName: canonical,
			Payload:    map[string]any{"canonical_name": canonical, "aliases": aliases, "category": "general"},
			Confidence: confidenceFor(group), Reason: "未解析标签形成稳定语义簇，建议建立标准标签并保留原始别名", AffectedAssets: affected,
		})
	}
	return proposals
}

func Normalize(v string) string {
	v = strings.TrimSpace(strings.ToLower(v))
	var b strings.Builder
	lastSep := false
	for _, r := range v {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r > unicode.MaxASCII {
			b.WriteRune(r)
			lastSep = false
		} else if !lastSep {
			b.WriteByte('_')
			lastSep = true
		}
	}
	return strings.Trim(b.String(), "_")
}

func semanticKey(v string) string {
	n := Normalize(v)
	replacements := map[string]string{
		"city_at_night": "urban_night", "night_city": "urban_night", "city_night": "urban_night",
		"夜间城市": "urban_night", "城市夜景": "urban_night", "夜晚城市": "urban_night",
		"shopfront": "storefront", "store_front": "storefront", "店铺": "storefront", "商店": "storefront",
		"behind_the_scene": "behind_the_scenes", "behind_scenes": "behind_the_scenes",
	}
	if mapped, ok := replacements[n]; ok {
		return mapped
	}
	// Strip a trailing "s" to collapse regular English plurals (cars→car).
	// Do not strip when the word ends in "ss" (class, glass) or when it is
	// short enough that the trailing 's' is unlikely to be a plural marker
	// (lens, bus, news). This heuristic errs on the side of keeping the 's'
	// rather than over-stemming, because semanticKey is a grouping hint and
	// two variants of the same word will still be surfaced to the curator.
	if strings.HasSuffix(n, "ss") || len(n) <= 4 {
		return n
	}
	return strings.TrimSuffix(n, "s")
}

func chooseCanonical(group []domain.UnresolvedTag) string {
	best := group[0].NormalizedTag
	bestScore := -1
	for _, item := range group {
		score := item.UsageCount
		if isASCII(item.NormalizedTag) {
			score += 2
		}
		if !strings.Contains(item.NormalizedTag, "__") {
			score++
		}
		if score > bestScore {
			best, bestScore = item.NormalizedTag, score
		}
	}
	return semanticKey(best)
}
func confidenceFor(group []domain.UnresolvedTag) float64 {
	if len(group) == 1 {
		return .72
	}
	if len(group) == 2 {
		return .86
	}
	return .93
}
func isASCII(v string) bool {
	for _, r := range v {
		if r > unicode.MaxASCII {
			return false
		}
	}
	return true
}
