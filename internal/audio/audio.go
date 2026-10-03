// Package audio switches the default Windows playback device using the
// MMDevice API for enumeration and the undocumented IPolicyConfig COM
// interface (the same one used by Volume2, EarTrumpet, AudioSwitcher, etc.)
// for changing the system default.
//
// All COM calls happen on a single dedicated OS thread (the worker
// goroutine) because the COM objects are created in that thread's
// apartment; exec() serializes every request onto it.
package audio

import (
	"errors"
	"runtime"

	"github.com/diegosz/go-wca/pkg/wca"
	"github.com/go-ole/go-ole"
)

// Device is a playback endpoint exposed to the UI layer.
type Device struct {
	ID   string
	Name string
}

// Switcher owns the COM objects needed to list and change the default
// playback device.
type Switcher struct {
	reqCh chan func()
	quit  chan struct{}

	mmde   *wca.IMMDeviceEnumerator
	policy *wca.IPolicyConfigVista
}

// NewSwitcher starts the worker goroutine, initializes COM on it and
// creates the long-lived COM objects. It blocks until initialization
// finishes (or fails).
func NewSwitcher() (*Switcher, error) {
	s := &Switcher{
		reqCh: make(chan func()),
		quit:  make(chan struct{}),
	}

	errCh := make(chan error, 1)
	go s.run(errCh)

	if err := <-errCh; err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Switcher) run(errCh chan error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if err := ole.CoInitializeEx(0, ole.COINIT_APARTMENTTHREADED); err != nil {
		errCh <- err
		return
	}
	defer ole.CoUninitialize()

	var mmde *wca.IMMDeviceEnumerator
	if err := wca.CoCreateInstance(wca.CLSID_MMDeviceEnumerator, 0, wca.CLSCTX_ALL, wca.IID_IMMDeviceEnumerator, &mmde); err != nil {
		errCh <- err
		return
	}
	defer mmde.Release()

	var policy *wca.IPolicyConfigVista
	if err := wca.CoCreateInstance(wca.GUID_CPolicyConfigVistaClient, 0, wca.CLSCTX_ALL, wca.GUID_IPolicyConfigVista, &policy); err != nil {
		errCh <- err
		return
	}
	defer policy.Release()

	s.mmde = mmde
	s.policy = policy
	errCh <- nil

	for {
		select {
		case f := <-s.reqCh:
			f()
		case <-s.quit:
			return
		}
	}
}

// exec runs f on the worker goroutine/thread and waits for it to finish.
func (s *Switcher) exec(f func() error) error {
	done := make(chan error, 1)
	s.reqCh <- func() { done <- f() }
	return <-done
}

// Close stops the worker goroutine and releases every COM object.
func (s *Switcher) Close() {
	close(s.quit)
}

// ListActiveDevices returns every active playback endpoint.
func (s *Switcher) ListActiveDevices() ([]Device, error) {
	var devices []Device
	err := s.exec(func() error {
		var err error
		devices, err = s.listActiveDevicesLocked()
		return err
	})
	return devices, err
}

// DefaultDeviceID returns the ID of the current default playback endpoint
// (console role).
func (s *Switcher) DefaultDeviceID() (string, error) {
	var id string
	err := s.exec(func() error {
		var err error
		id, err = s.defaultDeviceIDLocked()
		return err
	})
	return id, err
}

// SetDefault makes the device with the given ID the default endpoint for
// every role (console, multimedia, communications), matching what
// Volume2-style switchers do.
func (s *Switcher) SetDefault(id string) error {
	return s.exec(func() error {
		return s.setDefaultLocked(id)
	})
}

// Cycle moves the default device forward (direction > 0) or backward
// (direction < 0) through the list of active devices and returns the newly
// selected device. The whole read-modify-write happens as a single
// operation on the worker goroutine so it cannot race with a concurrent
// menu click or hotkey press.
func (s *Switcher) Cycle(direction int) (Device, error) {
	var result Device
	err := s.exec(func() error {
		devices, err := s.listActiveDevicesLocked()
		if err != nil {
			return err
		}
		if len(devices) == 0 {
			return errors.New("no active playback devices")
		}

		currentID, err := s.defaultDeviceIDLocked()
		if err != nil {
			return err
		}

		idx := 0
		for i, d := range devices {
			if d.ID == currentID {
				idx = i
				break
			}
		}

		next := ((idx+direction)%len(devices) + len(devices)) % len(devices)
		if err := s.setDefaultLocked(devices[next].ID); err != nil {
			return err
		}
		result = devices[next]
		return nil
	})
	return result, err
}

// --- internal helpers; must only be called from the worker goroutine ---

func (s *Switcher) listActiveDevicesLocked() ([]Device, error) {
	var collection *wca.IMMDeviceCollection
	if err := s.mmde.EnumAudioEndpoints(wca.ERender, wca.DEVICE_STATE_ACTIVE, &collection); err != nil {
		return nil, err
	}
	defer collection.Release()

	var count uint32
	if err := collection.GetCount(&count); err != nil {
		return nil, err
	}

	devices := make([]Device, 0, count)
	for i := uint32(0); i < count; i++ {
		var dev *wca.IMMDevice
		if err := collection.Item(i, &dev); err != nil {
			return nil, err
		}

		id, idErr := deviceID(dev)
		name, nameErr := deviceName(dev)
		dev.Release()

		if idErr != nil {
			return nil, idErr
		}
		if nameErr != nil {
			return nil, nameErr
		}
		devices = append(devices, Device{ID: id, Name: name})
	}
	return devices, nil
}

func (s *Switcher) defaultDeviceIDLocked() (string, error) {
	var dev *wca.IMMDevice
	if err := s.mmde.GetDefaultAudioEndpoint(wca.ERender, wca.EConsole, &dev); err != nil {
		return "", err
	}
	defer dev.Release()

	var id string
	if err := dev.GetId(&id); err != nil {
		return "", err
	}
	return id, nil
}

func (s *Switcher) setDefaultLocked(id string) error {
	for _, role := range []wca.ERole{wca.EConsole, wca.EMultimedia, wca.ECommunications} {
		if err := s.policy.SetDefaultEndpoint(id, role); err != nil {
			return err
		}
	}
	return nil
}

func deviceID(dev *wca.IMMDevice) (string, error) {
	var id string
	if err := dev.GetId(&id); err != nil {
		return "", err
	}
	return id, nil
}

func deviceName(dev *wca.IMMDevice) (string, error) {
	var ps *wca.IPropertyStore
	if err := dev.OpenPropertyStore(wca.STGM_READ, &ps); err != nil {
		return "", err
	}
	defer ps.Release()

	var pv wca.PROPVARIANT
	if err := ps.GetValue(&wca.PKEY_Device_FriendlyName, &pv); err != nil {
		return "", err
	}
	return pv.String(), nil
}
