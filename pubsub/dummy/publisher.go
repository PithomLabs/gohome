package dummy

import "github.com/barnybug/gohome/pubsub"

// Dummy Publisher for testing
type Publisher struct {
	Events []*pubsub.Event
}

func (self *Publisher) ID() string {
	return "dummy"
}

func (self *Publisher) Emit(ev *pubsub.Event) {
	self.Events = append(self.Events, ev)
}

func (self *Publisher) Close() {}
