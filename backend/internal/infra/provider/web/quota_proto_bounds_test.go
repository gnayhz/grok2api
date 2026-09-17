package web

import (
	"encoding/binary"
	"encoding/json"
	"math"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
)

func quotaTimestampFixture(seconds, nanos uint64) []byte {
	message := protowire.AppendTag(nil, 1, protowire.VarintType)
	message = protowire.AppendVarint(message, seconds)
	message = protowire.AppendTag(message, 2, protowire.VarintType)
	return protowire.AppendVarint(message, nanos)
}

func TestQuotaTimestampRejectsWireOverflowBeforeNarrowing(t *testing.T) {
	for _, test := range []struct {
		name           string
		seconds, nanos uint64
	}{
		{"nanoseconds_wrap_to_zero", 1800000000, 1 << 32},
		{"nanoseconds_wrap_to_valid_fraction", 1800000000, 1<<32 + 1},
		{"nanoseconds_out_of_range", 1800000000, 1000000000},
		{"seconds_after_year_9999", 253402300800, 0},
		{"seconds_signed_overflow", math.MaxUint64, 0},
		{"zero_seconds", 0, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			if value, err := parseProtoTimestamp(quotaTimestampFixture(test.seconds, test.nanos)); err == nil {
				t.Fatalf("invalid timestamp accepted: seconds=%d nanos=%d value=%v", test.seconds, test.nanos, value)
			}
		})
	}
	for _, seconds := range []uint64{1, 1800000000, 253402300799} {
		for _, nanos := range []uint64{0, 999999999} {
			value, err := parseProtoTimestamp(quotaTimestampFixture(seconds, nanos))
			if err != nil || value.Unix() != int64(seconds) || value.Nanosecond() != int(nanos) {
				t.Fatalf("valid timestamp changed: %v %v", value, err)
			}
			if _, err := json.Marshal(value); err != nil {
				t.Fatalf("accepted timestamp cannot be returned to an API client: %v", err)
			}
		}
	}
}

func TestWeeklyQuotaRejectsInvalidPeriodInsteadOfPublishingSnapshot(t *testing.T) {
	for _, test := range []struct{ seconds, nanos uint64 }{
		{1800000000, 1<<32 + 1},
		{253402300800, 1},
	} {
		quota := protowire.AppendTag(nil, 1, protowire.Fixed32Type)
		quota = protowire.AppendFixed32(quota, math.Float32bits(25))
		for _, field := range []protowire.Number{4, 5} {
			seconds := test.seconds
			if field == 5 {
				seconds += 7 * 24 * 60 * 60
			}
			quota = protowire.AppendTag(quota, field, protowire.BytesType)
			quota = protowire.AppendBytes(quota, quotaTimestampFixture(seconds, test.nanos))
		}
		message := protowire.AppendTag(nil, 1, protowire.BytesType)
		message = protowire.AppendBytes(message, quota)
		frame := binary.BigEndian.AppendUint32([]byte{0}, uint32(len(message)))
		frame = append(frame, message...)
		if _, err := parseWeeklyCreditsResponse(frame, 42, time.Now()); err == nil {
			t.Fatalf("invalid period published as a quota window: %+v", test)
		}
	}
}
