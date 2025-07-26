package gorge

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/orgball2608/gorge/gorge/internal/payload"
)

func TestJSONSerializer(t *testing.T) {
	s := &JSONSerializer{}

	t.Run("marshal and unmarshal successfully", func(t *testing.T) {
		p := &payload.CachePayload[string]{
			Data:      "hello world",
		}

		data, err := s.Marshal(p)
		assert.NoError(t, err)
		assert.NotEmpty(t, data)

		var newP payload.CachePayload[string]
		err = s.Unmarshal(data, &newP)
		assert.NoError(t, err)

		assert.Equal(t, p.Data, newP.Data)
	})

	t.Run("marshal and unmarshal with nil value", func(t *testing.T) {
		p := &payload.CachePayload[string]{
			IsNil:      true,
		}

		data, err := s.Marshal(p)
		assert.NoError(t, err)
		assert.NotEmpty(t, data)

		var newP payload.CachePayload[string]
		err = s.Unmarshal(data, &newP)
		assert.NoError(t, err)
		assert.Equal(t, p.IsNil, newP.IsNil)
	})

	t.Run("unmarshal with bad data", func(t *testing.T) {
		badData := []byte("this is not a json")
		var newP payload.CachePayload[string]
		err := s.Unmarshal(badData, &newP)
		assert.Error(t, err)
	})
}

