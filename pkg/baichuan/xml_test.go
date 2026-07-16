package baichuan

import "testing"

func FuzzXMLParsers(f *testing.F) {
	f.Add([]byte(`<body><Encryption><nonce>nonce</nonce></Encryption></body>`))
	f.Add([]byte(`<Extension><binaryData>1</binaryData><encryptLen>8</encryptLen></Extension>`))
	f.Add([]byte(`<body><TalkAbility version="1.1"><duplexList><duplex>fullDuplex</duplex></duplexList></TalkAbility></body>`))
	f.Add([]byte(`<body><AbilityInfo><system><subModule><abilityValue>version_ro</abilityValue></subModule></system></AbilityInfo></body>`))
	f.Fuzz(func(t *testing.T, body []byte) {
		_, _ = parseNonce(body)
		_, _ = parseExtension(body)
		_, _ = decodeTalkAbility(body)
		_, _ = parseCapabilities(body, 0)
		_, _ = parseDeviceInfo(body)
	})
}
