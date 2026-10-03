// Command volume_switch sits in the system tray and lets F7/F8 cycle the
// default Windows playback device, the same way Volume2 does.
package main

import (
	"log"
	"sync"

	"github.com/getlantern/systray"

	"volume_switch/internal/audio"
	"volume_switch/internal/hotkey"
	"volume_switch/internal/notify"
	"volume_switch/internal/trayicon"
)

func main() {
	systray.Run(onReady, onExit)
}

var (
	switcher *audio.Switcher
	hk       *hotkey.Listener

	menuMu     sync.Mutex
	menuItems  []*systray.MenuItem
	deviceByID map[string]int
)

func onReady() {
	systray.SetIcon(trayicon.Data)
	systray.SetTitle("")
	systray.SetTooltip("Volume Switch")

	var err error
	switcher, err = audio.NewSwitcher()
	if err != nil {
		log.Printf("audio init failed: %v", err)
		systray.SetTooltip("Volume Switch: audio init failed")
		return
	}

	devices, err := switcher.ListActiveDevices()
	if err != nil {
		log.Printf("list devices failed: %v", err)
	}
	currentID, err := switcher.DefaultDeviceID()
	if err != nil {
		log.Printf("get default device failed: %v", err)
	}

	buildDeviceMenu(devices, currentID)

	systray.AddSeparator()
	mQuit := systray.AddMenuItem("Выход", "Закрыть Volume Switch")
	go func() {
		<-mQuit.ClickedCh
		systray.Quit()
	}()

	hk = &hotkey.Listener{
		OnF7: func() { onCycle(-1) },
		OnF8: func() { onCycle(1) },
	}
	if err := hk.Start(); err != nil {
		log.Printf("hotkey hook failed: %v", err)
	}
}

func onExit() {
	if hk != nil {
		hk.Stop()
	}
	if switcher != nil {
		switcher.Close()
	}
}

func buildDeviceMenu(devices []audio.Device, currentID string) {
	menuMu.Lock()
	defer menuMu.Unlock()

	deviceByID = make(map[string]int, len(devices))
	menuItems = make([]*systray.MenuItem, len(devices))

	for i, dev := range devices {
		item := systray.AddMenuItemCheckbox(dev.Name, "Сделать устройством по умолчанию", dev.ID == currentID)
		menuItems[i] = item
		deviceByID[dev.ID] = i

		idx := i
		go func() {
			for range item.ClickedCh {
				selectDevice(devices[idx])
			}
		}()
	}
}

func selectDevice(dev audio.Device) {
	if err := switcher.SetDefault(dev.ID); err != nil {
		log.Printf("set default failed: %v", err)
		return
	}
	setChecked(dev.ID)
	announceSwitch(dev)
}

func onCycle(direction int) {
	dev, err := switcher.Cycle(direction)
	if err != nil {
		log.Printf("cycle failed: %v", err)
		return
	}
	setChecked(dev.ID)
	announceSwitch(dev)
}

func announceSwitch(dev audio.Device) {
	systray.SetTooltip("Volume Switch: " + dev.Name)
	notify.Show("Аудиовыход переключён", dev.Name)
}

func setChecked(id string) {
	menuMu.Lock()
	defer menuMu.Unlock()

	for _, item := range menuItems {
		item.Uncheck()
	}
	if idx, ok := deviceByID[id]; ok {
		menuItems[idx].Check()
	}
}
