package search

// RetrievalProfile is the per-intent wiring of the two weight surfaces the
// engine owns:
//
//	FieldWeights  — the lexical channel's FTS5 column weights, indexed
//	                [description, tags, objects, actions, mood].
//	ChannelWeights — the weighted-blend weights per retrieval channel.
//	                (RRF ignores these; it ranks per channel.)
//
// A speech query should rank transcript evidence above everything; a fact
// query should rank structured observation fields above narrative; a mood
// query should rank description/mood over objects. These profiles are the
// starting measurement, not the final word: the benchmark sweeps them.
type RetrievalProfile struct {
	FieldWeights   [5]float64
	ChannelWeights map[string]float64
}

// Profiles returns the default profile per intent. The auto profile mirrors
// the legacy blend's channel split so the compatibility behaviour and the v2
// auto behaviour agree where they overlap.
func Profiles() map[SearchIntent]RetrievalProfile {
	flat := [5]float64{1, 1, 1, 1, 1}
	return map[SearchIntent]RetrievalProfile{
		IntentAuto: {
			FieldWeights: flat,
			ChannelWeights: map[string]float64{
				SignalLexical:           0.30,
				SignalHeuristicSemantic: 0.70,
			},
		},
		IntentFact: {
			FieldWeights: [5]float64{1, 1, 1.2, 1.2, 0.8},
			ChannelWeights: map[string]float64{
				SignalLexical:           0.30,
				SignalHeuristicSemantic: 0.40,
				SignalTranscript:        0.10,
				SignalMetadata:          0.05,
				SignalTextEmbedding:     0.15,
			},
		},
		IntentSpeech: {
			FieldWeights: flat,
			ChannelWeights: map[string]float64{
				SignalTranscript:        0.60,
				SignalLexical:           0.25,
				SignalHeuristicSemantic: 0.15,
			},
		},
		IntentSemantic: {
			FieldWeights: flat,
			ChannelWeights: map[string]float64{
				SignalHeuristicSemantic: 0.45,
				SignalTextEmbedding:     0.35,
				SignalLexical:           0.20,
			},
		},
		IntentCreative: {
			FieldWeights: flat,
			ChannelWeights: map[string]float64{
				SignalLexical:           0.30,
				SignalHeuristicSemantic: 0.45,
				SignalTranscript:        0.05,
				SignalMetadata:          0.20,
			},
		},
		IntentSimilar: {
			FieldWeights: flat,
			ChannelWeights: map[string]float64{
				SignalHeuristicSemantic: 0.45,
				SignalTextEmbedding:     0.35,
				SignalLexical:           0.20,
			},
		},
	}
}
