package app

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/evjohn-icu/timingdex/internal/curator"
	"github.com/evjohn-icu/timingdex/internal/domain"
)

// RunTagEmbeddingClusters uses a real embedding provider only to discover
// candidate groups. It stores the vectors and transparent cluster membership,
// then sends each group through the existing proposal/review boundary.
func (s *Service) RunTagEmbeddingClusters(ctx context.Context, limit int, threshold float64) (domain.TagClusterResult, error) {
	if s.embedder == nil {
		return domain.TagClusterResult{}, fmt.Errorf("embedding provider is not configured; enable providers.embedding and set embedding_primary")
	}
	if limit <= 0 {
		limit = 500
	}
	if threshold <= 0 || threshold >= 1 {
		threshold = 0.86
	}
	items, err := s.repo.ListUnresolvedTags(ctx, limit)
	if err != nil {
		return domain.TagClusterResult{}, err
	}
	if len(items) < 2 {
		return domain.TagClusterResult{Strategy: s.embedder.Name() + ":" + s.embedder.Model(), TagsEmbedded: len(items)}, nil
	}
	inputs := make([]string, len(items))
	for i, item := range items {
		inputs[i] = item.NormalizedTag
	}
	vectors, err := s.embedder.Embed(ctx, inputs)
	if err != nil {
		return domain.TagClusterResult{}, fmt.Errorf("embed raw tags: %w", err)
	}
	if len(vectors) != len(items) {
		return domain.TagClusterResult{}, fmt.Errorf("embedding provider returned %d vectors for %d tags", len(vectors), len(items))
	}
	if err := s.repo.UpsertTagEmbeddings(ctx, s.embedder.Model(), items, vectors); err != nil {
		return domain.TagClusterResult{}, err
	}
	clusters := buildClusters(items, vectors, threshold)
	runID, err := s.repo.CreateTagClusterRun(ctx, s.embedder.Name(), s.embedder.Model(), threshold, clusters, len(items))
	if err != nil {
		return domain.TagClusterResult{}, err
	}

	existing, err := s.repo.ListCanonicalTags(ctx)
	if err != nil {
		return domain.TagClusterResult{}, err
	}
	byName := make(map[string]domain.UnresolvedTag, len(items))
	for _, item := range items {
		byName[item.NormalizedTag] = item
	}
	var proposals []domain.TagProposal
	strategy := "embedding:" + s.embedder.Name() + ":" + s.embedder.Model()
	for _, cluster := range clusters {
		group := make([]domain.UnresolvedTag, 0, len(cluster.Members))
		for _, member := range cluster.Members {
			group = append(group, byName[member])
		}
		var generated []domain.TagProposal
		if s.curator != nil {
			generated, err = s.curator.Curate(ctx, group, existing)
			if err == nil {
				strategy += "+" + s.curator.Name() + ":" + s.curator.Model()
			}
		}
		if s.curator == nil || err != nil {
			if err != nil && !s.cfg.Providers.TagCuratorFallbackHeuristic {
				return domain.TagClusterResult{}, fmt.Errorf("curate embedding cluster: %w", err)
			}
			generated = curator.BuildProposals(group, existing)
		}
		for i := range generated {
			generated[i].Reason = "Embedding candidate cluster (average similarity " + fmt.Sprintf("%.2f", cluster.Similarity) + "): " + generated[i].Reason
		}
		proposals = append(proposals, generated...)
	}
	created, err := s.repo.CreateTagCurationRun(ctx, proposals, len(items), strategy)
	if err != nil {
		return domain.TagClusterResult{}, err
	}
	return domain.TagClusterResult{RunID: runID, Strategy: strategy, TagsEmbedded: len(items), ClustersFound: len(clusters), ProposalsCreated: created.ProposalsCreated}, nil
}

func buildClusters(items []domain.UnresolvedTag, vectors [][]float64, threshold float64) []domain.TagCluster {
	parent := make([]int, len(items))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(x int) int {
		if parent[x] != x {
			parent[x] = find(parent[x])
		}
		return parent[x]
	}
	join := func(a, b int) {
		a, b = find(a), find(b)
		if a != b {
			parent[b] = a
		}
	}
	for i := 0; i < len(items); i++ {
		for j := i + 1; j < len(items); j++ {
			sim := cosine(vectors[i], vectors[j])
			if sim >= threshold {
				join(i, j)
			}
		}
	}
	groups := map[int][]int{}
	for i := range items {
		groups[find(i)] = append(groups[find(i)], i)
	}
	clusters := make([]domain.TagCluster, 0, len(groups))
	for _, indexes := range groups {
		if len(indexes) < 2 {
			continue
		}
		members := make([]string, 0, len(indexes))
		var total float64
		pairs := 0
		for _, index := range indexes {
			members = append(members, items[index].NormalizedTag)
		}
		for i := 0; i < len(indexes); i++ {
			for j := i + 1; j < len(indexes); j++ {
				total += cosine(vectors[indexes[i]], vectors[indexes[j]])
				pairs++
			}
		}
		sort.Strings(members)
		clusters = append(clusters, domain.TagCluster{Members: members, Similarity: total / float64(pairs), State: "candidate"})
	}
	sort.Slice(clusters, func(i, j int) bool {
		return strings.Join(clusters[i].Members, "|") < strings.Join(clusters[j].Members, "|")
	})
	return clusters
}

func cosine(a, b []float64) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return -1
	}
	var dot, aa, bb float64
	for i := range a {
		dot += a[i] * b[i]
		aa += a[i] * a[i]
		bb += b[i] * b[i]
	}
	if aa == 0 || bb == 0 {
		return -1
	}
	return dot / math.Sqrt(aa*bb)
}

func (s *Service) GenerateLibrarySummary(ctx context.Context) (domain.LibrarySummary, error) {
	input, err := s.repo.BuildLibrarySummaryInput(ctx)
	if err != nil {
		return domain.LibrarySummary{}, err
	}
	var draft domain.LibrarySummaryDraft
	provider, model := "deterministic", "library-summary-v1"
	if s.curator != nil {
		draft, err = s.curator.SummarizeLibrary(ctx, input)
		if err == nil {
			provider, model = s.curator.Name(), s.curator.Model()
		}
	}
	if s.curator == nil || err != nil {
		if err != nil && !s.cfg.Providers.TagCuratorFallbackHeuristic {
			return domain.LibrarySummary{}, fmt.Errorf("generate library summary: %w", err)
		}
		draft = deterministicLibrarySummary(input)
	}
	return s.repo.SaveLibrarySummary(ctx, domain.LibrarySummary{Scope: "all", Summary: draft.Summary, Themes: draft.Themes, SuitableFor: draft.SuitableFor, Input: input, Provider: provider, Model: model})
}

func deterministicLibrarySummary(input domain.LibrarySummaryInput) domain.LibrarySummaryDraft {
	themes := make([]string, 0, 6)
	for _, tag := range input.TopTags {
		themes = append(themes, tag.CanonicalName)
		if len(themes) == 6 {
			break
		}
	}
	summary := fmt.Sprintf("当前素材库共有 %d 条可用素材，已完成内容分析 %d 条", input.AssetCount, input.AnalyzedCount)
	if len(themes) > 0 {
		summary += "。高频语义集中在：" + strings.Join(themes, "、") + "。"
	} else {
		summary += "。尚未形成足够的已治理标签，建议先完成 Tag Curator 审核。"
	}
	return domain.LibrarySummaryDraft{Summary: summary, Themes: themes, SuitableFor: []string{"按主题回收 B-roll", "建立旧素材翻新候选集", "按场景检索可复用片段"}}
}
