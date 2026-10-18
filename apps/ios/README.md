# iOS host

SwiftUI + AVFoundation. Same `/v1` protocol as Android and the web host. Needs macOS with Xcode 26+ and [XcodeGen](https://github.com/yonaskolb/XcodeGen).

The hub is untrusted (see `docs/protocol.md`). `Link.swift` ports `internal/link` with CryptoKit; debug builds check it against the pinned vectors at launch (`LinkCheck.swift`). Pairing shows the SAS for the user to confirm, and the resulting link key is kept in the Keychain (this device only) and never sent to a hub. The app connects to the saved hub plus hubs found on the Wi-Fi (at most 4), each socket with its own agent session, and keeps one connection per `hubId`, preferring the saved URL.

```
cd apps/ios
xcodegen generate
# compile check
xcodebuild -project Ambivalence.xcodeproj -scheme Ambivalence -sdk iphonesimulator \
  -destination "generic/platform=iOS Simulator" build
# unsigned device build (BrowserStack re-signs uploads itself)
xcodebuild -project Ambivalence.xcodeproj -scheme Ambivalence -sdk iphoneos -configuration Release \
  -destination "generic/platform=iOS" -derivedDataPath build/dev CODE_SIGNING_ALLOWED=NO build
mkdir -p Payload && cp -R build/dev/Build/Products/Release-iphoneos/Ambivalence.app Payload/
zip -qry ambivalence-unsigned.ipa Payload
```

## Sideload signing

The repo bundle id is `dev.micr.ambivalence`. To install on your own iPhone with a provisioning profile that forces another bundle id, re-sign the built `.app`:

1. Import the `.p12` into a throwaway keychain and add that keychain to the user search list for the duration of signing. Without that, codesign fails with `errSecInternalComponent`.
2. Set `CFBundleIdentifier` in `Payload/Ambivalence.app/Info.plist` to the profile's app id.
3. Copy the profile to `Payload/Ambivalence.app/embedded.mobileprovision`.
4. Run `codesign -f -s <SHA1> --keychain <kc> --entitlements <profile entitlements> --generate-entitlement-der Payload/Ambivalence.app`.
5. Restore the search list and delete the keychain.

Never commit the `.p12` or the profile.
