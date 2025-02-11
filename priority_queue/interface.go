package priorityqueue

type Queue interface {
	Insert(item Item)
	ExtractMin() Item
	Len() uint64
}

// Item represents binary heap item
type Item interface {
	// ID is a unique item identifier
	ID() string

	// Priority returns the Item's priority to sort
	Priority() int64

	// Body is the Item payload
	Body() []byte

	// Context is the Item meta information
	Context() ([]byte, error)

	// Ack - acknowledge the Item after processing
	Ack() error

	// Nack - discard the Item
	Nack() error

	// Requeue - put the message back to the queue with the optional delay
	Requeue(headers map[string][]string, delay int64) error
}
