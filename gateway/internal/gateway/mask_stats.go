package gateway

func mergeMaskStats(dst *maskAggregate, src maskAggregate) {
	if dst == nil {
		return
	}
	if dst.byLabel == nil {
		dst.byLabel = map[string]int{}
	}
	if dst.byWord == nil {
		dst.byWord = map[string]map[string]int{}
	}
	dst.total += src.total
	for label, count := range src.byLabel {
		dst.byLabel[label] += count
	}
	for label, words := range src.byWord {
		if dst.byWord[label] == nil {
			dst.byWord[label] = map[string]int{}
		}
		for word, count := range words {
			dst.byWord[label][word] += count
		}
	}
}
