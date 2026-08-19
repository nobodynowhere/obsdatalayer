package rewrite

// PayloadStats describes what label rewriting observed in a write payload.
// ItemKind is "series" for Mimir remote write and "streams" for Loki push.
type PayloadStats struct {
	ItemKind          string
	ItemsTotal        int
	ItemsModified     int
	LabelsDropped     int
	LabelsInjected    int
	LabelsOverwritten int
}

func (s PayloadStats) Empty() bool {
	return s.ItemKind == "" || s.ItemsTotal == 0
}

func (s *PayloadStats) AddItem(before, after map[string]string) {
	s.ItemsTotal++
	dropped, injected, overwritten := diffLabels(before, after)
	if dropped > 0 || injected > 0 || overwritten > 0 {
		s.ItemsModified++
	}
	s.LabelsDropped += dropped
	s.LabelsInjected += injected
	s.LabelsOverwritten += overwritten
}

func diffLabels(before, after map[string]string) (dropped, injected, overwritten int) {
	for key, beforeValue := range before {
		afterValue, ok := after[key]
		if !ok {
			dropped++
			continue
		}
		if afterValue != beforeValue {
			overwritten++
		}
	}
	for key := range after {
		if _, ok := before[key]; !ok {
			injected++
		}
	}
	return dropped, injected, overwritten
}
