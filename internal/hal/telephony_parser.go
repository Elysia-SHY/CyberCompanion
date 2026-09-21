package hal

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// CellularInfo 表示蜂窝网络及信号采集结果。
type CellularInfo struct {
	NetworkType  string `json:"network_type"`
	SignalRSRP   string `json:"signal_rsrp"`
	SignalDetail string `json:"signal_detail"`
	SignalBar    int    `json:"signal_bar"`
	Operator     string `json:"operator"`
	Band         string `json:"band"`
	RSRP         int    `json:"rsrp"`
	RSRQ         int    `json:"rsrq"`
	SINR         int    `json:"sinr"`
}

var (
	reNrRSRP     = regexp.MustCompile(`ssRsrp\s*=\s*(-?\d+)`)
	reNrRSRQ     = regexp.MustCompile(`ssRsrq\s*=\s*(-?\d+)`)
	reNrSINR     = regexp.MustCompile(`ssSinr\s*=\s*(-?\d+)`)
	reNrLevel    = regexp.MustCompile(`level\s*=\s*(\d+)`)
	reLteRSRP    = regexp.MustCompile(`rsrp=(-?\d+)`)
	reLteRSRQ    = regexp.MustCompile(`rsrq=(-?\d+)`)
	reLteRSSNR   = regexp.MustCompile(`rssnr=(-?\d+)`)
	reLteLevel   = regexp.MustCompile(`level=(\d+)`)
	reBand       = regexp.MustCompile(`mBands\s*=\s*\[([0-9,\s]+)\]`)
	reBandSingle = regexp.MustCompile(`mBand\s*=\s*(\d+)`)
	reOperator   = regexp.MustCompile(`mOperatorAlphaLong(?:Raw)?\s*=\s*([^,\n]+)`)
	reRadioTech  = regexp.MustCompile(`getRilDataRadioTechnology=\d+\(([^)]+)\)`)
)

// parseDumpsysTelephonyOutput 解析 dumpsys telephony.registry 的输出文本。
func parseDumpsysTelephonyOutput(out string) *CellularInfo {
	if len(out) == 0 {
		return nil
	}

	info := &CellularInfo{
		RSRP: 2147483647,
		RSRQ: 2147483647,
		SINR: 2147483647,
	}

	// 1. 运营商
	if m := reOperator.FindStringSubmatch(out); len(m) > 1 {
		op := strings.TrimSpace(m[1])
		if op != "" && op != "null" {
			info.Operator = op
		}
	}

	// 2. 无线电接入制式
	tech := ""
	if m := reRadioTech.FindStringSubmatch(out); len(m) > 1 {
		tech = strings.TrimSpace(m[1])
	}

	// 3. 频段 (Band)
	if m := reBand.FindStringSubmatch(out); len(m) > 1 {
		rawBands := strings.Split(m[1], ",")
		for _, b := range rawBands {
			b = strings.TrimSpace(b)
			if b != "" && b != "0" {
				if strings.Contains(strings.ToUpper(tech), "NR") {
					info.Band = "n" + b
				} else {
					info.Band = "B" + b
				}
				break
			}
		}
	} else if m := reBandSingle.FindStringSubmatch(out); len(m) > 1 {
		b := strings.TrimSpace(m[1])
		if b != "" && b != "0" {
			if strings.Contains(strings.ToUpper(tech), "NR") {
				info.Band = "n" + b
			} else {
				info.Band = "B" + b
			}
		}
	}

	// 4. 信号强度 (扫描所有 mSignalStrength)
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "mSignalStrength=SignalStrength:") {
			continue
		}

		is5G := strings.Contains(line, "primary=CellSignalStrengthNr") ||
			(!strings.Contains(line, "primary=CellSignalStrengthLte") && strings.Contains(line, "ssRsrp"))

		if is5G {
			if m := reNrRSRP.FindStringSubmatch(line); len(m) > 1 {
				if v, err := strconv.Atoi(m[1]); err == nil && v < 0 && v != 2147483647 {
					info.RSRP = v
				}
			}
			if m := reNrRSRQ.FindStringSubmatch(line); len(m) > 1 {
				if v, err := strconv.Atoi(m[1]); err == nil && v < 0 && v != 2147483647 {
					info.RSRQ = v
				}
			}
			if m := reNrSINR.FindStringSubmatch(line); len(m) > 1 {
				if v, err := strconv.Atoi(m[1]); err == nil && v != 2147483647 {
					info.SINR = v
				}
			}
			if m := reNrLevel.FindStringSubmatch(line); len(m) > 1 {
				if v, err := strconv.Atoi(m[1]); err == nil {
					info.SignalBar = v
				}
			}
		}

		if info.RSRP == 2147483647 {
			if m := reLteRSRP.FindStringSubmatch(line); len(m) > 1 {
				if v, err := strconv.Atoi(m[1]); err == nil && v < 0 && v != 2147483647 {
					info.RSRP = v
				}
			}
			if m := reLteRSRQ.FindStringSubmatch(line); len(m) > 1 {
				if v, err := strconv.Atoi(m[1]); err == nil && v < 0 && v != 2147483647 {
					info.RSRQ = v
				}
			}
			if m := reLteRSSNR.FindStringSubmatch(line); len(m) > 1 {
				if v, err := strconv.Atoi(m[1]); err == nil && v != 2147483647 {
					info.SINR = v
				}
			}
			if m := reLteLevel.FindStringSubmatch(line); len(m) > 1 {
				if v, err := strconv.Atoi(m[1]); err == nil {
					info.SignalBar = v
				}
			}
		}

		if info.RSRP != 2147483647 {
			break
		}
	}

	// 5. 补充扫描：若 mSignalStrength 中未给出有效 RSRQ / SINR，从 mCellInfo 中提取
	if info.RSRQ == 2147483647 {
		for _, m := range reNrRSRQ.FindAllStringSubmatch(out, -1) {
			if len(m) > 1 {
				if v, err := strconv.Atoi(m[1]); err == nil && v != 2147483647 {
					info.RSRQ = v
					break
				}
			}
		}
		if info.RSRQ == 2147483647 {
			for _, m := range reLteRSRQ.FindAllStringSubmatch(out, -1) {
				if len(m) > 1 {
					if v, err := strconv.Atoi(m[1]); err == nil && v != 2147483647 {
						info.RSRQ = v
						break
					}
				}
			}
		}
	}
	if info.SINR == 2147483647 {
		for _, m := range reNrSINR.FindAllStringSubmatch(out, -1) {
			if len(m) > 1 {
				if v, err := strconv.Atoi(m[1]); err == nil && v != 2147483647 {
					info.SINR = v
					break
				}
			}
		}
		if info.SINR == 2147483647 {
			for _, m := range reLteRSSNR.FindAllStringSubmatch(out, -1) {
				if len(m) > 1 {
					if v, err := strconv.Atoi(m[1]); err == nil && v != 2147483647 {
						info.SINR = v
						break
					}
				}
			}
		}
	}

	// 6. 构造信号质量字符串与格数
	if info.RSRP != 2147483647 {
		quality := rsrpQuality(info.RSRP)
		info.SignalRSRP = fmt.Sprintf("%d dBm (%s)", info.RSRP, quality)

		detailParts := []string{fmt.Sprintf("%d dBm", info.RSRP)}
		if info.RSRQ != 2147483647 {
			detailParts = append(detailParts, fmt.Sprintf("RSRQ: %d dB", info.RSRQ))
		}
		if info.SINR != 2147483647 {
			detailParts = append(detailParts, fmt.Sprintf("SINR: %d dB", info.SINR))
		}
		detailParts = append(detailParts, quality)
		info.SignalDetail = strings.Join(detailParts, " · ")

		if info.SignalBar <= 0 {
			info.SignalBar = rsrpToBars(info.RSRP)
		}
	}

	// 7. 构造网络制式完整描述 (如 "中国电信 · 5G NR (SA) (n78)")
	techLabel := formatTech(tech)
	netParts := []string{}
	if info.Operator != "" {
		netParts = append(netParts, info.Operator)
	}
	if techLabel != "" {
		if info.Band != "" {
			netParts = append(netParts, fmt.Sprintf("%s (%s)", techLabel, info.Band))
		} else {
			netParts = append(netParts, techLabel)
		}
	} else if info.Band != "" {
		netParts = append(netParts, info.Band)
	}
	if len(netParts) > 0 {
		info.NetworkType = strings.Join(netParts, " · ")
	} else {
		info.NetworkType = "蜂窝移动网络"
	}

	return info
}

func rsrpQuality(rsrp int) string {
	switch {
	case rsrp >= -80:
		return "极佳"
	case rsrp >= -90:
		return "良好"
	case rsrp >= -105:
		return "一般"
	case rsrp >= -115:
		return "较弱"
	default:
		return "极弱"
	}
}

func rsrpToBars(rsrp int) int {
	switch {
	case rsrp >= -80:
		return 5
	case rsrp >= -90:
		return 4
	case rsrp >= -100:
		return 3
	case rsrp >= -110:
		return 2
	case rsrp >= -120:
		return 1
	default:
		return 0
	}
}

func formatTech(tech string) string {
	t := strings.ToUpper(strings.TrimSpace(tech))
	switch {
	case strings.Contains(t, "NR_SA"):
		return "5G NR (SA)"
	case strings.Contains(t, "NR_NSA"):
		return "5G NR (NSA)"
	case strings.Contains(t, "NR"):
		return "5G NR"
	case strings.Contains(t, "LTE_CA"):
		return "4G+ LTE-A"
	case strings.Contains(t, "LTE"):
		return "4G LTE"
	case strings.Contains(t, "UMTS") || strings.Contains(t, "HSPA") || strings.Contains(t, "WCDMA"):
		return "3G WCDMA"
	case strings.Contains(t, "GSM") || strings.Contains(t, "EDGE") || strings.Contains(t, "GPRS"):
		return "2G GSM"
	default:
		if tech != "" && tech != "UNKNOWN" {
			return tech
		}
		return "蜂窝网络"
	}
}
