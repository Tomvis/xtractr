package xtractr

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestLooksLikeLatinMojibake(t *testing.T) {
	t.Parallel()

	assert.True(t, looksLikeLatinMojibake([]byte(`TITLE "Ïåñíÿ áåç ñëîâ (÷åðíîâèê)"`)))
	assert.False(t, looksLikeLatinMojibake([]byte(`TITLE "Песня без слов (черновик)"`)))
	assert.False(t, looksLikeLatinMojibake([]byte(`PERFORMER "Björk"`+"\n"+`TITLE "Café Müller à l'été"`)))
	assert.False(t, looksLikeLatinMojibake([]byte(`TITLE "Plain ASCII"`)))
}
