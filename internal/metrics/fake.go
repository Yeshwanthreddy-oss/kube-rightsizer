package metrics

import "context"

// key uniquely identifies a container within FakeSource's fixture data.
type key struct {
	namespace, workload, container string
}

// FakeSource is an in-memory UsageSource for tests and the local demo. It
// never touches the network, making it the offline stand-in for a real
// Prometheus during unit tests and controller-runtime fake-client tests.
type FakeSource struct {
	cpu map[key][]float64
	mem map[key][]float64
	// Err, if set, is returned by every call regardless of key, to
	// exercise error handling paths.
	Err error
}

// NewFakeSource returns an empty FakeSource ready for SetCPU/SetMemory.
func NewFakeSource() *FakeSource {
	return &FakeSource{cpu: map[key][]float64{}, mem: map[key][]float64{}}
}

// SetCPU registers CPU millicore samples for a namespace/workload/container.
func (f *FakeSource) SetCPU(namespace, workload, container string, samples []float64) {
	f.cpu[key{namespace, workload, container}] = samples
}

// SetMemory registers memory byte samples for a namespace/workload/container.
func (f *FakeSource) SetMemory(namespace, workload, container string, samples []float64) {
	f.mem[key{namespace, workload, container}] = samples
}

func (f *FakeSource) CPUMillicoreSamples(ctx context.Context, q Query) ([]float64, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	samples, ok := f.cpu[key{q.Namespace, q.Workload, q.Container}]
	if !ok || len(samples) == 0 {
		return nil, &ErrNoData{Query: q}
	}
	return samples, nil
}

func (f *FakeSource) MemoryByteSamples(ctx context.Context, q Query) ([]float64, error) {
	if f.Err != nil {
		return nil, f.Err
	}
	samples, ok := f.mem[key{q.Namespace, q.Workload, q.Container}]
	if !ok || len(samples) == 0 {
		return nil, &ErrNoData{Query: q}
	}
	return samples, nil
}

var _ UsageSource = (*FakeSource)(nil)
