package trace

import "encoding/binary"

// The OTLP trace request, encoded by hand in protobuf's wire format so the
// service needs no OpenTelemetry or protobuf dependency. Field numbers are
// those of opentelemetry/proto/collector/trace/v1 and trace/v1.

type kv struct {
	key string
	// one of
	s     string
	i     int64
	isInt bool
}

func str(k, v string) kv       { return kv{key: k, s: v} }
func num(k string, v int64) kv { return kv{key: k, i: v, isInt: true} }

type span struct {
	traceID  [16]byte
	spanID   [8]byte
	parentID [8]byte // zero for the root
	name     string
	start    int64 // Unix nanoseconds
	end      int64
	attrs    []kv
	failed   bool
	message  string
}

type buf []byte

func (b *buf) varint(v uint64)     { *b = binary.AppendUvarint(*b, v) }
func (b *buf) tag(field, wire int) { b.varint(uint64(field<<3 | wire)) }

func (b *buf) bytes(field int, p []byte) {
	b.tag(field, 2)
	b.varint(uint64(len(p)))
	*b = append(*b, p...)
}

func (b *buf) string(field int, s string) {
	b.tag(field, 2)
	b.varint(uint64(len(s)))
	*b = append(*b, s...)
}

func (b *buf) fixed64(field int, v uint64) {
	b.tag(field, 1)
	*b = binary.LittleEndian.AppendUint64(*b, v)
}

func (b *buf) uint(field int, v uint64) {
	b.tag(field, 0)
	b.varint(v)
}

// message appends a nested message built by f.
func (b *buf) message(field int, f func(*buf)) {
	var m buf
	f(&m)
	b.bytes(field, m)
}

func (b *buf) keyValue(field int, a kv) {
	b.message(field, func(m *buf) {
		m.string(1, a.key)
		m.message(2, func(v *buf) { // AnyValue
			if a.isInt {
				v.uint(3, uint64(a.i))
			} else {
				v.string(1, a.s)
			}
		})
	})
}

// encode builds an ExportTraceServiceRequest with one resource and one
// scope.
func encode(resource []kv, scope, version string, spans []span) []byte {
	var req buf
	req.message(1, func(rs *buf) { // ResourceSpans
		rs.message(1, func(r *buf) { // Resource
			for _, a := range resource {
				r.keyValue(1, a)
			}
		})
		rs.message(2, func(ss *buf) { // ScopeSpans
			ss.message(1, func(s *buf) { // InstrumentationScope
				s.string(1, scope)
				if version != "" {
					s.string(2, version)
				}
			})
			for _, sp := range spans {
				ss.message(2, func(m *buf) { // Span
					m.bytes(1, sp.traceID[:])
					m.bytes(2, sp.spanID[:])
					if sp.parentID != ([8]byte{}) {
						m.bytes(4, sp.parentID[:])
					}
					m.string(5, sp.name)
					m.uint(6, 1) // SPAN_KIND_INTERNAL
					m.fixed64(7, uint64(sp.start))
					m.fixed64(8, uint64(max(sp.end, sp.start)))
					for _, a := range sp.attrs {
						m.keyValue(9, a)
					}
					m.message(15, func(st *buf) { // Status
						if sp.failed {
							if sp.message != "" {
								st.string(2, sp.message)
							}
							st.uint(3, 2) // STATUS_CODE_ERROR
						} else {
							st.uint(3, 1) // STATUS_CODE_OK
						}
					})
				})
			}
		})
	})
	return req
}
