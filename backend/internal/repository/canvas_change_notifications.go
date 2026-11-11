package repository

type canvasChange struct{ userID, canvasID string }

// Transaction descendants collect invalidations until the outer commit succeeds.
func (r *Repository) WithCanvasChangeNotifier(notify func(string, string)) *Repository {
	copy := *r
	copy.canvasChangeNotifier = notify
	return &copy
}

func (r *Repository) canvasChanged(userID, canvasID string) {
	if r.canvasChangeNotifier == nil {
		return
	}
	if r.canvasChanges != nil {
		r.canvasChanges[canvasChange{userID, canvasID}] = struct{}{}
		return
	}
	r.canvasChangeNotifier(userID, canvasID)
}

func (r *Repository) WithWakeNotifier(notify func(string)) *Repository {
	copy := *r
	copy.wakeNotifier = notify
	return &copy
}

func (r *Repository) HasWakeNotifier() bool {
	return r != nil && r.wakeNotifier != nil
}

func (r *Repository) wakeChanged(topic string) {
	if r == nil || r.wakeNotifier == nil || topic == "" {
		return
	}
	if r.wakeTopics != nil {
		r.wakeTopics[topic] = struct{}{}
		return
	}
	r.wakeNotifier(topic)
}
