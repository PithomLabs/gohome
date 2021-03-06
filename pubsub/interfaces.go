package pubsub

// Publisher interface
type Publisher interface {
	ID() string
	Emit(ev *Event)
	Close()
}

// Subscriber interface
type Subscriber interface {
	ID() string
	FilteredChannel(...string) <-chan *Event
	Channel() <-chan *Event
	Close(<-chan *Event)
}
