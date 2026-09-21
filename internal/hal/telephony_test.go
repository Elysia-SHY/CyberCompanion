package hal

import (
	"testing"
)

func TestParseDumpsysTelephonySample(t *testing.T) {
	// Sample dump from real Flymodem U20 5G
	sample := `last known state:
  Phone Id=0
    mServiceState={mOperatorAlphaLong=中国电信, getRilDataRadioTechnology=20(NR_SA)}
    mSignalStrength=SignalStrength:{mCdma=CellSignalStrengthCdma: cdmaDbm=2147483647,mLte=CellSignalStrengthLte: rsrp=2147483647,mNr=CellSignalStrengthNr:{ csiRsrp = 2147483647 csiRsrq = 2147483647 ssRsrp = -73 ssRsrq = 2147483647 ssSinr = 2147483647 level = 3 },primary=CellSignalStrengthNr}
    mCellInfo=[CellInfoNr:{ mRegistered=YES CellIdentityNr:{ mPci = 879 mTac = 6970371 mBands = [78, 78] mMcc = 460 mMnc = 11 } CellSignalStrengthNr:{ ssRsrp = -72 ssRsrq = -6 ssSinr = 13 level = 3 } }]`

	info := parseDumpsysTelephonyOutput(sample)
	if info == nil {
		t.Fatal("parseDumpsysTelephonyOutput returned nil")
	}

	if info.Operator != "中国电信" {
		t.Errorf("expected operator 中国电信, got %q", info.Operator)
	}
	if info.Band != "n78" {
		t.Errorf("expected band n78, got %q", info.Band)
	}
	if info.RSRP != -73 {
		t.Errorf("expected RSRP -73, got %d", info.RSRP)
	}
	if info.RSRQ != -6 {
		t.Errorf("expected RSRQ -6, got %d", info.RSRQ)
	}
	if info.SINR != 13 {
		t.Errorf("expected SINR 13, got %d", info.SINR)
	}
	if info.SignalBar != 3 && info.SignalBar != 4 {
		t.Errorf("expected SignalBar 3 or 4, got %d", info.SignalBar)
	}
	if info.NetworkType != "中国电信 · 5G NR (SA) (n78)" {
		t.Errorf("expected network type 中国电信 · 5G NR (SA) (n78), got %q", info.NetworkType)
	}
}

func TestParseDumpsysTelephonyLTESample(t *testing.T) {
	// Sample dump from 4G LTE
	sample := `last known state:
  Phone Id=0
    mServiceState={mOperatorAlphaLong=中国移动, getRilDataRadioTechnology=14(LTE)}
    mSignalStrength=SignalStrength:{mLte=CellSignalStrengthLte: rssi=-65 rsrp=-92 rsrq=-9 rssnr=180 level=4,primary=CellSignalStrengthLte}
    mCellInfo=[CellInfoLte:{ mRegistered=YES CellIdentityLte:{ mPci = 123 mTac = 45678 mBand = 3 } }]`

	info := parseDumpsysTelephonyOutput(sample)
	if info == nil {
		t.Fatal("parseDumpsysTelephonyOutput returned nil")
	}

	if info.Operator != "中国移动" {
		t.Errorf("expected operator 中国移动, got %q", info.Operator)
	}
	if info.Band != "B3" {
		t.Errorf("expected band B3, got %q", info.Band)
	}
	if info.RSRP != -92 {
		t.Errorf("expected RSRP -92, got %d", info.RSRP)
	}
	if info.RSRQ != -9 {
		t.Errorf("expected RSRQ -9, got %d", info.RSRQ)
	}
	if info.SINR != 180 {
		t.Errorf("expected SINR 180, got %d", info.SINR)
	}
	if info.NetworkType != "中国移动 · 4G LTE (B3)" {
		t.Errorf("expected network type 中国移动 · 4G LTE (B3), got %q", info.NetworkType)
	}
}
