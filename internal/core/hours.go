package core

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

var dayKeys = []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}

var dayNamesRu = []string{"понедельник", "вторник", "среду", "четверг", "пятницу", "субботу", "воскресенье"}

func dayIndex(s string) (int, bool) {
	for i, d := range dayKeys {
		if d == s {
			return i, true
		}
	}
	return 0, false
}

// parseDays understands "mon", "mon-fri", "sat,sun" and "mon-thu,sun".
func parseDays(key string) ([]int, error) {
	var out []int
	for _, part := range strings.Split(key, ",") {
		part = strings.ToLower(strings.TrimSpace(part))
		if a, b, isRange := strings.Cut(part, "-"); isRange {
			from, ok1 := dayIndex(strings.TrimSpace(a))
			to, ok2 := dayIndex(strings.TrimSpace(b))
			if !ok1 || !ok2 || from > to {
				return nil, fmt.Errorf("«%s» — ожидается диапазон дней вида mon-fri", part)
			}
			for i := from; i <= to; i++ {
				out = append(out, i)
			}
			continue
		}
		i, ok := dayIndex(part)
		if !ok {
			return nil, fmt.Errorf("«%s» — дни недели пишутся как mon, tue, wed, thu, fri, sat, sun", part)
		}
		out = append(out, i)
	}
	return out, nil
}

// ParseClock reads "HH:MM" as minutes since midnight; "24:00" (1440) is
// an end of day. The one clock parser of the gateway: drivers use it for
// the CRM's clock times.
func ParseClock(s string) (int, bool) {
	h, m, ok := strings.Cut(strings.TrimSpace(s), ":")
	if !ok || len(h) == 0 || len(h) > 2 || len(m) != 2 {
		return 0, false
	}
	hh, err1 := strconv.Atoi(h)
	mm, err2 := strconv.Atoi(m)
	if err1 != nil || err2 != nil || hh < 0 || mm < 0 || mm > 59 || hh > 24 || (hh == 24 && mm != 0) {
		return 0, false
	}
	return hh*60 + mm, true
}

// FormatClock writes minutes since midnight as "HH:MM".
func FormatClock(min int) string {
	return fmt.Sprintf("%02d:%02d", min/60, min%60)
}

func parseHours(path string, m map[string]string) ([7]*[2]int, []string) {
	var hours [7]*[2]int
	var defined [7]bool
	var errs []string
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := strings.TrimSpace(m[k])
		days, err := parseDays(k)
		if err != nil {
			errs = append(errs, path+": "+err.Error())
			continue
		}
		var dh *[2]int
		if lv := strings.ToLower(v); lv != "closed" {
			a, b, ok := strings.Cut(v, "-")
			open, ok1 := ParseClock(a)
			closeAt, ok2 := ParseClock(b)
			if !ok || !ok1 || !ok2 || open >= 1440 {
				errs = append(errs, fmt.Sprintf("%s.%s: «%s» — ожидается время вида \"07:00-23:00\" (конец дня — 24:00) или closed", path, k, v))
				continue
			}
			if closeAt <= open {
				errs = append(errs, fmt.Sprintf("%s.%s: «%s» — закрытие должно быть позже открытия; работа после полуночи пока не поддерживается, укажите 24:00", path, k, v))
				continue
			}
			dh = &[2]int{open, closeAt}
		}
		for _, d := range days {
			if defined[d] {
				errs = append(errs, fmt.Sprintf("%s: часы на %s указаны дважды", path, dayNamesRu[d]))
				continue
			}
			defined[d] = true
			hours[d] = dh
		}
	}
	for i, ok := range defined {
		if !ok {
			errs = append(errs, fmt.Sprintf("%s: не указаны часы на %s (если клуб не работает — укажите closed)", path, dayNamesRu[i]))
		}
	}
	return hours, errs
}
