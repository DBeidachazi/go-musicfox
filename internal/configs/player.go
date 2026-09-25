package configs

import "github.com/go-musicfox/netease-music/service"

type PlayerOptions struct {
	Engine          string // 播放引擎
	BeepMp3Decoder  string // beep mp3解码器
	MpdBin          string // mpd路径
	MpdConfigFile   string // mpd配置文件
	MpdNetwork      string // mpd网络类型: tcp、unix
	MpdAddr         string // mpd地址
	MpdAutoStart    bool   // mpd自动启动
	MaxPlayErrCount int    // 最大错误重试次数
	MpvBin          string // mpv路径
}

// PlayerConfig 播放器引擎与行为配置
type PlayerConfig struct {
	// 播放引擎
	Engine string `koanf:"engine"`
	// 最大错误重试次数
	MaxPlayErrCount int `koanf:"maxPlayErrCount"`
	// 歌曲音质级别
	SongLevel service.SongQualityLevel `koanf:"songLevel"`
	// 显示歌单下所有歌曲
	ShowAllSongsOfPlaylist bool `koanf:"showAllSongsOfPlaylist"`

	// 鼠标滚轮单次滚动调节的音量值 (取值范围 1-20)
	MouseVolumeStep int `koanf:"mouseVolumeStep"`

	Beep BeepConfig `koanf:"beep"`
	Mpd  MpdConfig  `koanf:"mpd"`
	Mpv  MpvConfig  `koanf:"mpv"`
	Dlna DlnaConfig `koanf:"dlna"`
}

// BeepConfig `beep` 引擎专属配置
type BeepConfig struct {
	// beep mp3解码器
	Mp3Decoder string `koanf:"mp3Decoder"`
	// 是否启用无缝播放
	Gapless bool `koanf:"gapless"`
	// 提前多少秒预加载下一首
	GaplessPreloadSeconds int `koanf:"gaplessPreloadSeconds"`
	// 歌曲间过渡: off / crossfade / automix
	Automix string `koanf:"automix"`
	// crossfade 模式的过渡秒数 (1-25)
	CrossfadeSeconds int `koanf:"crossfadeSeconds"`
	// 过渡模式下提前多少秒预加载并分析下一首
	AutomixPreloadSeconds int `koanf:"automixPreloadSeconds"`
	// automix 按需下载的模型: none / beat / full
	AutomixModels string `koanf:"automixModels"`
	// 模型目录，空为 <data>/automix/models
	AutomixModelsDir string `koanf:"automixModelsDir"`
	// 额外的下载镜像，%s 代表文件名
	AutomixModelMirrors []string `koanf:"automixModelMirrors"`
	// 自备的 Python（装有 onnxruntime、numpy），用于音轨分离
	AutomixPython string `koanf:"automixPython"`
	// 自备的 onnxruntime 动态库
	AutomixOrtLibrary string `koanf:"automixOrtLibrary"`
	// 音轨分离后端: auto / python / native
	AutomixStemBackend string `koanf:"automixStemBackend"`
	// 推理线程数，0 为逻辑核数的四分之一
	AutomixThreads int `koanf:"automixThreads"`
}

// MpdConfig `mpd` 引擎专属配置
type MpdConfig struct {
	// mpd路径
	Bin string `koanf:"bin"`
	// mpd配置文件
	ConfigFile string `koanf:"configFile"`
	// mpd网络类型: tcp、unix
	Network string `koanf:"network"`
	// mpd地址
	Addr string `koanf:"addr"`
	// mpd自动启动
	AutoStart bool `koanf:"autoStart"`
}

// MpvConfig `mpv` 引擎专属配置
type MpvConfig struct {
	// mpv路径
	Bin string `koanf:"bin"`
}

// DlnaConfig `dlna` 引擎专属配置
type DlnaConfig struct {
	DeviceUrl string `koanf:"deviceUrl"`
	LocalIP   string `koanf:"localIP"`
}
