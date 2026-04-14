package audio

import (
	"fmt"
	"sync"

	"github.com/gordonklaus/portaudio"
)

var paMu sync.Mutex

func InitializePortAudio() error {
	paMu.Lock()
	defer paMu.Unlock()
	return portaudio.Initialize()
}

func TerminatePortAudio() error {
	paMu.Lock()
	defer paMu.Unlock()
	return portaudio.Terminate()
}

func ReinitPortAudio() error {
	paMu.Lock()
	defer paMu.Unlock()

	termErr := portaudio.Terminate()
	initErr := portaudio.Initialize()
	if initErr != nil {
		if termErr != nil {
			return fmt.Errorf("reinitialize portaudio: terminate: %v; initialize: %w", termErr, initErr)
		}
		return fmt.Errorf("reinitialize portaudio: %w", initErr)
	}

	return nil
}
