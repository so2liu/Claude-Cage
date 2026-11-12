package exitpolicy

import (
	"errors"
	"net/netip"
	"regexp"
	"strings"
	"time"
)

// Policy is user configuration. Its anchor survives checks and restarts;
// observations must never silently replace a configured address or region.
type Policy struct {
	Mode        string `json:"mode"`
	Scope       string `json:"scope"`
	IP          string `json:"ip,omitempty"`
	CountryCode string `json:"country_code,omitempty"`
	Region      string `json:"region,omitempty"`
	City        string `json:"city,omitempty"`
}

type Location struct {
	IP, CountryCode, Region, City string
	Country, RegionCode, Timezone string
	CheckedAt                     time.Time
}

type State string

const (
	Passed  State = "passed"
	Failed  State = "failed"
	Unknown State = "unknown"
)

func (p Policy) Normalized() Policy {
	if p.Mode == "" {
		p.Mode = "dynamic"
	}
	if p.Scope == "" {
		p.Scope = "country"
	}
	p.IP = strings.TrimSpace(p.IP)
	p.CountryCode = strings.ToUpper(strings.TrimSpace(p.CountryCode))
	p.Region = strings.Join(strings.Fields(p.Region), " ")
	p.City = strings.Join(strings.Fields(p.City), " ")
	return p
}

func (p Policy) Validate() error {
	p = p.Normalized()
	if p.Mode != "static" && p.Mode != "dynamic" {
		return errors.New("请选择静态或动态 IP")
	}
	if p.Scope != "any" && p.Scope != "country" && p.Scope != "region" && p.Scope != "city" {
		return errors.New("请选择动态 IP 的允许范围")
	}
	if p.IP != "" {
		ip, err := netip.ParseAddr(p.IP)
		if err != nil || !ip.Is4() || !ip.IsGlobalUnicast() || ip.IsPrivate() {
			return errors.New("固定出口应填写公网 IPv4 地址")
		}
	}
	if p.CountryCode != "" && !regexp.MustCompile(`^[A-Z]{2}$`).MatchString(p.CountryCode) {
		return errors.New("国家或地区代码应为两个字母，例如 US")
	}
	if len(p.Region) > 120 || len(p.City) > 120 {
		return errors.New("地区名称过长")
	}
	return nil
}

func (p Policy) NeedsBinding() bool {
	p = p.Normalized()
	if p.Mode == "static" {
		return p.IP == ""
	}
	return p.Scope != "any" && (p.CountryCode == "" || (p.Scope == "region" || p.Scope == "city") && p.Region == "" || p.Scope == "city" && p.City == "")
}

// BindMissing is used only for a previously unconfigured anchor. It leaves
// existing values intact, including when the new observation violates them.
func (p Policy) BindMissing(location Location) Policy {
	p = p.Normalized()
	if state, _ := CheckExitCountry(location.IP, &location); state != Passed {
		return p
	}
	if p.Mode == "static" && p.IP == "" {
		p.IP = location.IP
	}
	if p.Mode == "dynamic" && p.Scope != "any" {
		if p.CountryCode == "" {
			p.CountryCode = location.CountryCode
		}
		if (p.Scope == "region" || p.Scope == "city") && strings.EqualFold(p.CountryCode, location.CountryCode) {
			if p.Region == "" {
				p.Region = location.Region
			}
			if p.Scope == "city" && p.City == "" {
				p.City = location.City
			}
		}
	}
	return p.Normalized()
}

func (p Policy) Evaluate(ip string, location *Location) (State, string) {
	p = p.Normalized()
	if err := p.Validate(); err != nil {
		return Unknown, err.Error()
	}
	if state, message := CheckExitCountry(ip, location); state != Passed {
		return state, message
	}
	if p.NeedsBinding() {
		if p.Mode == "dynamic" && p.CountryCode == "" {
			return Unknown, "动态 IP 的允许国家或地区尚未绑定，外网保持关闭。请在网络与代理设置中确认出口地区"
		}
		return Unknown, "尚未绑定出口规则，请在设置中确认检测到的 IP 或地区"
	}
	if p.Mode == "static" {
		if p.IP != ip {
			return Failed, "检测出口已偏离绑定的静态 IP"
		}
		return Passed, "检测出口与绑定的静态 IP 一致"
	}
	if p.Scope == "any" {
		return Passed, "动态 IP 可在非中国大陆地区变化，流量仍须经指定代理"
	}
	if !strings.EqualFold(p.CountryCode, location.CountryCode) {
		return Failed, "检测出口超出允许的国家或地区"
	}
	if p.Scope == "country" {
		return Passed, "检测出口位于允许的国家或地区"
	}
	if location.Region == "" {
		return Unknown, "当前位置缺少州或省，不能确认允许范围"
	}
	if !strings.EqualFold(p.Region, strings.Join(strings.Fields(location.Region), " ")) {
		return Failed, "检测出口超出允许的州或省范围"
	}
	if p.Scope == "region" {
		return Passed, "检测出口位于允许的州或省"
	}
	if location.City == "" {
		return Unknown, "当前位置缺少州、省或城市，不能确认城市范围"
	}
	if !strings.EqualFold(p.City, strings.Join(strings.Fields(location.City), " ")) {
		return Failed, "检测出口超出允许的城市范围"
	}
	return Passed, "检测出口位于允许的城市及其所属州、省"
}
