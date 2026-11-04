//go:build !baremetal

package bluetooth

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
)

var (
	_ GATTCService        = (*DeviceService)(nil)
	_ GATTCCharacteristic = (*DeviceCharacteristic)(nil)
)

var (
	errDupNotif = errors.New("unclosed notifications")
)

// notifyTimeout bounds the StartNotify and StopNotify calls. BlueZ doesn't
// always answer them when the link is failing, and without a deadline the
// caller would wait forever.
const notifyTimeout = 10 * time.Second

// UUIDWrapper is a type alias for UUID so we ensure no conflicts with
// struct method of the same name.
type uuidWrapper = UUID

// DeviceService is a BLE service on a connected peripheral device.
type DeviceService struct {
	uuidWrapper
	adapter     *Adapter
	servicePath string
}

// UUID returns the UUID for this DeviceService.
func (s DeviceService) UUID() UUID {
	return s.uuidWrapper
}

// DiscoverServices starts a service discovery procedure. Pass a list of service
// UUIDs you are interested in to this function. Either a slice of all services
// is returned (of the same length as the requested UUIDs and in the same
// order), or if some services could not be discovered an error is returned.
//
// Passing a nil slice of UUIDs will return a complete list of
// services.
//
// On Linux with BlueZ, this just waits for the ServicesResolved signal (if
// services haven't been resolved yet) and uses this list of cached services.
func (d Device) DiscoverServices(uuids []UUID) ([]DeviceService, error) {
	start := time.Now()

	for {
		resolved, err := d.device.GetProperty("org.bluez.Device1.ServicesResolved")
		if err != nil {
			return nil, err
		}
		if resolved.Value().(bool) {
			break
		}
		// This is a terrible hack, but I couldn't find another way.
		// TODO: actually there is, by waiting for a property change event of
		// ServicesResolved.
		time.Sleep(10 * time.Millisecond)
		if time.Since(start) > 10*time.Second {
			return nil, errors.New("timeout on DiscoverServices")
		}
	}

	services := []DeviceService{}
	uuidServices := make(map[UUID]struct{})
	servicesFound := 0

	// Iterate through all objects managed by BlueZ, hoping to find the services
	// we're looking for.
	var list map[dbus.ObjectPath]map[string]map[string]dbus.Variant
	err := d.adapter.bluez.Call("org.freedesktop.DBus.ObjectManager.GetManagedObjects", 0).Store(&list)
	if err != nil {
		return nil, err
	}
	objects := make([]string, 0, len(list))
	for objectPath := range list {
		objects = append(objects, string(objectPath))
	}
	sort.Strings(objects)
	for _, objectPath := range objects {
		if !strings.HasPrefix(objectPath, string(d.device.Path())+"/service") {
			continue
		}
		properties, ok := list[dbus.ObjectPath(objectPath)]["org.bluez.GattService1"]
		if !ok {
			continue
		}

		serviceUUID, _ := ParseUUID(properties["UUID"].Value().(string))

		if len(uuids) > 0 {
			found := false
			for _, uuid := range uuids {
				if uuid == serviceUUID {
					// One of the services we're looking for.
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}

		if _, ok := uuidServices[serviceUUID]; ok {
			// There is more than one service with the same UUID?
			// Don't overwrite it, to keep the servicesFound count correct.
			continue
		}

		ds := DeviceService{
			uuidWrapper: serviceUUID,
			adapter:     d.adapter,
			servicePath: objectPath,
		}

		services = append(services, ds)
		servicesFound++
		uuidServices[serviceUUID] = struct{}{}
	}

	if servicesFound < len(uuids) {
		return nil, ErrServiceNotFound
	}

	return services, nil
}

// DeviceCharacteristic is a BLE characteristic on a connected peripheral
// device.
type DeviceCharacteristic struct {
	uuidWrapper
	adapter                      *Adapter
	characteristic               dbus.BusObject
	property                     chan *dbus.Signal // channel where notifications are reported
	propertiesChangedMatchOption dbus.MatchOption  // the same value must be passed to RemoveMatchSignal
}

// UUID returns the UUID for this DeviceCharacteristic.
func (c DeviceCharacteristic) UUID() UUID {
	return c.uuidWrapper
}

// DiscoverCharacteristics discovers characteristics in this service. Pass a
// list of characteristic UUIDs you are interested in to this function. Either a
// list of all requested services is returned, or if some services could not be
// discovered an error is returned. If there is no error, the characteristics
// slice has the same length as the UUID slice with characteristics in the same
// order in the slice as in the requested UUID list.
//
// Passing a nil slice of UUIDs will return a complete
// list of characteristics.
func (s DeviceService) DiscoverCharacteristics(uuids []UUID) ([]DeviceCharacteristic, error) {
	var chars []DeviceCharacteristic
	if len(uuids) > 0 {
		// The caller wants to get a list of characteristics in a specific
		// order.
		chars = make([]DeviceCharacteristic, len(uuids))
	}

	// Iterate through all objects managed by BlueZ, hoping to find the
	// characteristic we're looking for.
	var list map[dbus.ObjectPath]map[string]map[string]dbus.Variant
	err := s.adapter.bluez.Call("org.freedesktop.DBus.ObjectManager.GetManagedObjects", 0).Store(&list)
	if err != nil {
		return nil, err
	}
	objects := make([]string, 0, len(list))
	for objectPath := range list {
		objects = append(objects, string(objectPath))
	}
	sort.Strings(objects)
	for _, objectPath := range objects {
		if !strings.HasPrefix(objectPath, s.servicePath+"/char") {
			continue
		}
		properties, ok := list[dbus.ObjectPath(objectPath)]["org.bluez.GattCharacteristic1"]
		if !ok {
			continue
		}
		cuuid, _ := ParseUUID(properties["UUID"].Value().(string))
		char := DeviceCharacteristic{
			uuidWrapper:    cuuid,
			adapter:        s.adapter,
			characteristic: s.adapter.bus.Object("org.bluez", dbus.ObjectPath(objectPath)),
		}

		if len(uuids) > 0 {
			// The caller wants to get a list of characteristics in a specific
			// order. Check whether this is one of those.
			for i, uuid := range uuids {
				if chars[i] != (DeviceCharacteristic{}) {
					// To support multiple identical characteristics, we need to
					// ignore the characteristics that are already found. See:
					// https://github.com/tinygo-org/bluetooth/issues/131
					continue
				}
				if cuuid == uuid {
					// one of the characteristics we're looking for.
					chars[i] = char
					break
				}
			}
		} else {
			// The caller wants to get all characteristics, in any order.
			chars = append(chars, char)
		}
	}

	// Check that we have found all characteristics.
	for _, char := range chars {
		if char == (DeviceCharacteristic{}) {
			return nil, ErrCharacteristicNotFound
		}
	}

	return chars, nil
}

// WriteWithoutResponse replaces the characteristic value with a new value. The
// call will return before all data has been written. A limited number of such
// writes can be in flight at any given time. This call is also known as a
// "write command" (as opposed to a write request).
func (c DeviceCharacteristic) WriteWithoutResponse(p []byte) (int, error) {
	args := map[string]any{"type": "command"}
	if err := c.characteristic.Call("org.bluez.GattCharacteristic1.WriteValue", 0, p, args).Err; err != nil {
		return 0, err
	}
	return len(p), nil
}

// Write replaces the characteristic value with a new value. The
// call will return after all data has been written.
func (c DeviceCharacteristic) Write(p []byte) (int, error) {
	args := map[string]any{"type": "request"}
	if err := c.characteristic.Call("org.bluez.GattCharacteristic1.WriteValue", 0, p, args).Err; err != nil {
		return 0, err
	}
	return len(p), nil
}

// EnableNotifications enables notifications in the Client Characteristic
// Configuration Descriptor (CCCD). This means that most peripherals will send a
// notification with a new value every time the value of the characteristic
// changes.
//
// Users may call EnableNotifications with a nil callback to disable notifications.
func (c *DeviceCharacteristic) EnableNotifications(callback func(buf []byte)) error {
	switch callback {
	default:
		if c.property != nil {
			return errDupNotif
		}

		// Start watching for changes in the Value property.
		property := make(chan *dbus.Signal)
		c.adapter.bus.Signal(property)
		matchOption := dbus.WithMatchInterface("org.freedesktop.DBus.Properties")
		c.adapter.bus.AddMatchSignal(matchOption)

		// Read the channel from the moment it is registered. godbus hands every
		// signal on the bus to every registered channel, and parks a goroutine
		// for each one a channel isn't ready to take, so a channel nobody reads
		// grows memory for as long as it stays registered.
		go func() {
			for sig := range property {
				if sig.Name == "org.freedesktop.DBus.Properties.PropertiesChanged" {
					interfaceName := sig.Body[0].(string)
					if interfaceName != "org.bluez.GattCharacteristic1" {
						continue
					}
					if sig.Path != c.characteristic.Path() {
						continue
					}
					changes := sig.Body[1].(map[string]dbus.Variant)
					if value, ok := changes["Value"].Value().([]byte); ok {
						callback(value)
					}
				}
			}
		}()

		ctx, cancel := context.WithTimeout(context.Background(), notifyTimeout)
		defer cancel()
		err := c.characteristic.CallWithContext(ctx, "org.bluez.GattCharacteristic1.StartNotify", 0).Err
		if err != nil {
			// Unregister the channel, or it stays registered for the life of
			// the connection to the bus with nothing to disable it.
			c.adapter.bus.RemoveMatchSignal(matchOption)
			c.adapter.bus.RemoveSignal(property)
			close(property)
			return err
		}

		c.property = property
		c.propertiesChangedMatchOption = matchOption
		return nil

	case nil:
		if c.property == nil {
			return nil
		}
		// Make the D-Bus call to stop notifications on the characteristic.
		ctx, cancel := context.WithTimeout(context.Background(), notifyTimeout)
		defer cancel()
		stopNotifyErr := c.characteristic.CallWithContext(ctx, "org.bluez.GattCharacteristic1.StopNotify", 0).Err
		// Still clean up other resources if there was an error
		removeSignalErr := c.adapter.bus.RemoveMatchSignal(c.propertiesChangedMatchOption)
		c.adapter.bus.RemoveSignal(c.property)
		close(c.property)
		c.property = nil

		// If there were errors, prioritize notify err
		if stopNotifyErr == nil {
			return removeSignalErr
		}
		return stopNotifyErr		
	}
}

// GetMTU returns the MTU for the characteristic.
func (c DeviceCharacteristic) GetMTU() (uint16, error) {
	mtu, err := c.characteristic.GetProperty("org.bluez.GattCharacteristic1.MTU")
	if err != nil {
		return uint16(0), err
	}
	return mtu.Value().(uint16), nil
}

// Read reads the current characteristic value.
func (c DeviceCharacteristic) Read(data []byte) (int, error) {
	options := make(map[string]interface{})
	var result []byte
	err := c.characteristic.Call("org.bluez.GattCharacteristic1.ReadValue", 0, options).Store(&result)
	if err != nil {
		return 0, err
	}
	return copyValue(data, result)
}
