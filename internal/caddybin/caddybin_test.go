package caddybin

import "testing"

func TestNewer(t *testing.T) {
	cases := []struct {
		cur, latest string
		want        bool
	}{
		{"v2.11.4", "v2.11.4", false}, // 一样，不该提示
		{"v2.11.3", "v2.11.4", true},
		{"v2.10.9", "v2.11.0", true},
		{"v1.9.9", "v2.0.0", true},
		{"2.11.3", "v2.11.4", true},   // 少个 v 也认
		{"v2.11.5", "v2.11.4", false}, // 本地比线上新（自己编译的），不提示
		{"v2.11.10", "v2.11.9", false},
		{"v2.11.9", "v2.11.10", true}, // 按数字比，不是按字典序
		{"", "v2.11.4", false},        // 读不出当前版本就别瞎提示
		{"v2.11.4", "", false},
		{"garbage", "v2.11.4", false},
		{"v2.11", "v2.11.4", false}, // 段数不对，宁可不提示
	}
	for _, c := range cases {
		if got := Newer(c.cur, c.latest); got != c.want {
			t.Errorf("Newer(%q, %q) = %v，期望 %v", c.cur, c.latest, got, c.want)
		}
	}
}

func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{
		"2.11.4": "v2.11.4", "v2.11.4": "v2.11.4", " v2.11.4 ": "v2.11.4", "": "",
	} {
		if got := normalize(in); got != want {
			t.Errorf("normalize(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// 助手脚本不在的机器上（比如开发机），必须如实报不可用，
// 而不是给个点了会炸的按钮。
func TestHelperUnavailableIsHonest(t *testing.T) {
	m := New("")
	ok, why := m.HelperAvailable()
	if ok && why != "" {
		t.Fatal("可用时不该带理由")
	}
	if !ok && why == "" {
		t.Fatal("不可用时必须说明原因，界面要显示给用户")
	}
}

// 拿不到权限时 StartUpgrade 必须直接失败，而不是留下一个卡在 running 的任务。
func TestStartUpgradeRefusesWithoutHelper(t *testing.T) {
	m := New("")
	if ok, _ := m.HelperAvailable(); ok {
		t.Skip("这台机器上助手可用，跳过")
	}
	if err := m.StartUpgrade(); err == nil {
		t.Fatal("助手不可用时 StartUpgrade 应该报错")
	}
	if job := m.Job(); job.State != StateIdle {
		t.Fatalf("失败后任务状态应保持 idle，实际是 %q", job.State)
	}
}
