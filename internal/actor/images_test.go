package actor

import (
	"strings"
	"testing"

	"github.com/cohesionhq/actor-codex/internal/cohesion"
)

func TestTidyAndSendable(t *testing.T) {
	tidied := Tidy(cohesion.Image{MediaType: " IMAGE/JPG ", Data: "data:image/jpeg;base64,iVBO\nRw0KGgo"})
	if tidied.MediaType != "image/jpeg" || tidied.Data != "iVBORw0KGgo=" {
		t.Errorf("got %+v", tidied)
	}
	images := []cohesion.Image{
		{MediaType: "image/png", Data: "iVBORw0KGgo="},
		{MediaType: "image/bmp", Data: "iVBORw0KGgo="},
		{MediaType: "image/png", Data: "not base64!"},
		{MediaType: "image/png", Data: strings.Repeat("A", MaxImageLength+4)},
	}
	for range 12 {
		images = append(images, cohesion.Image{MediaType: "image/png", Data: "iVBORw0KGgo="})
	}
	kept, dropped := Sendable(images)
	if len(kept) != MaxImages {
		t.Errorf("kept %d", len(kept))
	}
	if len(dropped) != 3+3 || dropped[0] != "it is image/bmp" || dropped[1] != "it isn't base64" || dropped[2] != "it is over 5 MB" {
		t.Errorf("dropped %v", dropped)
	}
}
