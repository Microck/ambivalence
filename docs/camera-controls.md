# Lens selection and manual still-camera controls

Call `camera_list` (CLI: `ambivalence cameras`) first. Select an exact `id` with
`camera_switch({"camera":"<id>"})` or `ambivalence switch '<id>'`. `front` and `back`
remain aliases for each side's default camera. Camera IDs are opaque and local to
the phone/browser; do not infer a lens from the ID or assume IDs work on another phone.

* iOS discovers physical wide, ultra-wide and telephoto devices and returns their
  unique IDs and localized names. Selecting a physical device avoids virtual camera
  lens switching. Only lenses present on the device appear.
* Android lists CameraX camera IDs plus Android 9+ physical cameras as
  `logicalId/physicalId`. Both preview and still output target the chosen physical
  camera. Manufacturers can hide lenses or reject a preview/JPEG stream combination;
  those lenses cannot be guaranteed accessible. Lens labels include focal lengths.
* Browsers list the video input devices the browser exposes. Selection uses an
  exact `deviceId`, never a facing-mode hint. Browser/OS policy may expose fewer
  cameras than the native app. Capabilities are available for the active input;
  select another input and list again to inspect it.

## Configure

MCP: `camera_configure` takes the fields below directly. CLI: pass a JSON object:

```sh
ambivalence configure '{"focusMode":"manual","focusPosition":0}'
ambivalence configure '{"focusMode":"auto","focusPoint":{"x":0.5,"y":0.5}}'
ambivalence configure '{"exposureMode":"manual","exposureTime":0.008,"iso":100}'
ambivalence configure '{"exposureMode":"auto","exposureCompensation":-0.5}'
ambivalence configure '{"whiteBalanceMode":"manual","whiteBalanceGains":{"red":2,"green":1,"blue":1.5}}'
ambivalence configure '{"whiteBalanceMode":"manual","whiteBalanceTemperature":5600}'
ambivalence configure '{"focusMode":"auto","exposureMode":"auto","whiteBalanceMode":"auto"}'
```

| Field | Meaning | Hosts |
| --- | --- | --- |
| `focusMode` | `auto` or `manual`; manual requires `focusPosition` | Where supported |
| `focusPosition` | 0 = farthest/infinity end, 1 = nearest end; device-relative, not a measured subject distance | Native apps; browser if focus distance is exposed |
| `focusPoint` | `{x,y}` in [0,1], native sensor coordinates; requires auto focus | Native apps where supported |
| `exposureMode` | `auto` or `manual`; manual requires both shutter and ISO | Where supported |
| `exposureTime` | Shutter duration in seconds | Where supported |
| `iso` | Sensor sensitivity; integer on Android | Where supported |
| `exposureCompensation` | EV bias in auto exposure; Android rounds to the nearest supported EV step | Where supported |
| `whiteBalanceMode` | `auto` or `manual` | Where supported |
| `whiteBalanceGains` | `{red,green,blue}`, each >= 1; equal green gains for both Bayer greens on Android | Native apps where supported |
| `whiteBalanceTemperature` | Kelvin; iOS uses zero tint, checks converted gains; browser uses colorTemperature | iOS and supported browsers |

Check the active camera's `capabilities` in `camera_list` for supported modes,
ranges and EV steps. `controlsVersion: 1` is required for `camera_configure`;
older apps return an upgrade error rather than silently ignoring controls.
Missing capabilities mean unsupported/unknown, never permission to assume support.
Android uses the MANUAL_SENSOR and MANUAL_POST_PROCESSING flags for shutter/ISO
and RGB controls. Fixed-focus lenses do not gain manual focusing through software.

Omitted control groups stay configured; explicitly switching a group to `auto`
clears its manual values. Lens changes start a new native camera configuration;
configure the lens after switching. An explicit mode is required when supplying
manual values. EV bias cannot be combined with manual exposure. Requests validate
all supplied control values before applying them; a later driver/browser failure
can still leave partial changes, so inspect settings before retrying.

Status and capture metadata include `cameraId` and `settings`. iOS reports device
settings after manual setters complete. Android reports the configuration accepted
by Camera2 (including requested EV, rounded by the driver); these are not measured
per-photo sensor results. Browser settings are read back after constraints apply;
an ignored or materially different constraint is an error. Automatic focus/exposure
may still be converging when a command completes. Native point coordinates are not
rotated portrait-preview coordinates.

The iOS still capture uses speed quality prioritization for custom exposure, as
required to preserve manual shutter/ISO. Browser capture remains a JPEG of the
video track. Browser support varies; unsupported focus points/RGB gains fail
explicitly. Browser focus position interpolates its exposed distance range in
reverse, so zero means its farthest supported setting, not guaranteed infinity.
Very long shutter times remain subject to existing command/capture RPC timeouts.
No video or frame-rate controls are added.

## API research

Implementation uses the platform APIs below rather than guessing hardware support:

* [Android CaptureRequest](https://developer.android.com/reference/android/hardware/camera2/CaptureRequest): AF modes/distance/regions, AE mode, nanosecond shutter, ISO, EV index and AWB/gains.
* [Android multi-camera](https://developer.android.com/media/camera/camera2/multi-camera): physical versus logical camera access and device limitations.
* [CameraX Camera2Interop.Extender](https://developer.android.com/reference/androidx/camera/camera2/interop/Camera2Interop.Extender): physical stream routing on both use cases (available in the pinned CameraX 1.4.1).
* [Camera2CameraControl](https://developer.android.com/reference/androidx/camera/camera2/interop/Camera2CameraControl): awaited capture-request options; these override CameraX's 3A defaults.
* [Apple custom focus](https://developer.apple.com/documentation/avfoundation/avcapturedevice/setfocusmodelocked(lensposition:completionhandler:)): normalized lens position and completion.
* [Apple custom exposure](https://developer.apple.com/documentation/avfoundation/avcapturedevice/setexposuremodecustom(duration:iso:completionhandler:)): shutter/ISO and photo quality prioritization.
* [Apple camera discovery](https://developer.apple.com/documentation/avfoundation/avcapturedevice/discoverysession): physical device types and unique IDs.
* [Apple white balance](https://developer.apple.com/documentation/avfoundation/avcapturedevice/setwhitebalancemodelocked(with:completionhandler:)): supported gains and locking.
* [W3C Image Capture](https://www.w3.org/TR/image-capture/): capability/constraint/settings contracts; exposure time uses 100-microsecond units, focus distance usually meters.

## Validation

`go test -race -count=1 ./...` includes sealed lens/configure/capture round trips,
zero-value preservation, independent auto resets, invalid modes and incomplete
manual exposure. `node --test scripts/test-browser-controls.cjs` checks browser
unit conversion, lens-position endpoints, unsupported controls and ignored
constraints. CI also builds Android and iOS. Real-device verification is still
required for hardware-specific lens availability, focus/exposure convergence,
physical-camera request behavior and the resulting JPEGs.
