package sdk

import (
	"context"
	"sync"
)

// MergeInputSources combines transport input streams into one Harness input stream.
func MergeInputSources(ctx context.Context, sources ...InputSource) (<-chan Input, error) {
	streams := make([]<-chan Input, 0, len(sources))
	for _, source := range sources {
		if source == nil {
			continue
		}
		stream, err := source.Receive(ctx)
		if err != nil {
			return nil, err
		}
		streams = append(streams, stream)
	}
	out := make(chan Input)
	var wg sync.WaitGroup
	wg.Add(len(streams))
	for _, stream := range streams {
		go func(stream <-chan Input) {
			defer wg.Done()
			for {
				select {
				case input, ok := <-stream:
					if !ok {
						return
					}
					select {
					case out <- input:
					case <-ctx.Done():
						return
					}
				case <-ctx.Done():
					return
				}
			}
		}(stream)
	}
	go func() {
		wg.Wait()
		close(out)
	}()
	return out, nil
}

// ChannelInputSource adapts an existing canonical input channel to InputSource.
type ChannelInputSource struct {
	Inputs <-chan Input
}

func (s ChannelInputSource) Receive(context.Context) (<-chan Input, error) {
	return s.Inputs, nil
}
