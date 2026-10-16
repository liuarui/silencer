/**
 * 全局壁纸与毛玻璃外观配置
 *
 * 设计取舍：
 * - 纯前端偏好（localStorage），不进后端 config.json；
 * - 默认内置旗舰预设「天境流光 (Ambient Flow)」：无实色纯黑/纯白底，纯透明背景 + 四角大半径弥散光晕，
 *   在深色模式下呈现深邃太空与科技星云，在浅色模式下呈现清澈日光与轻盈流光，完美兼顾深浅双模；
 * - 黄金比例参数：背景透明度 35%、背景虚化 12px、卡片不透明度 78%（兼顾透光与文字锐利度）。
 */

export const DEFAULT_OPACITY = 60
export const DEFAULT_BLUR = 6
export const DEFAULT_CARD_OPACITY = 45

/**
 * 旗舰自适应流动柔光矢量 SVG（深浅自适应·全屏饱满弥散光斑·免网络离线可用）
 * 左上天青、右上深靛、中央梦幻紫罗兰、右下翡翠绿、左下海洋蓝，全屏无死角流光交织
 */
export const DEFAULT_WALLPAPER_DATA_URL = `data:image/svg+xml;utf8,${encodeURIComponent(`
<svg xmlns="http://www.w3.org/2000/svg" width="1920" height="1080" viewBox="0 0 1920 1080" fill="none">
  <defs>
    <!-- 左上至中心：明澈天青与科技电光蓝 -->
    <radialGradient id="amb-cyan" cx="20%" cy="25%" r="75%">
      <stop offset="0%" stop-color="#0284c7" stop-opacity="0.95"/>
      <stop offset="35%" stop-color="#06b6d4" stop-opacity="0.65"/>
      <stop offset="70%" stop-color="#0ea5e9" stop-opacity="0.25"/>
      <stop offset="100%" stop-color="#0ea5e9" stop-opacity="0"/>
    </radialGradient>
    <!-- 右上至中心偏右：深邃星云靛蓝与皇家紫 -->
    <radialGradient id="amb-indigo" cx="78%" cy="28%" r="80%">
      <stop offset="0%" stop-color="#4f46e5" stop-opacity="0.95"/>
      <stop offset="40%" stop-color="#6366f1" stop-opacity="0.7"/>
      <stop offset="75%" stop-color="#818cf8" stop-opacity="0.3"/>
      <stop offset="100%" stop-color="#4f46e5" stop-opacity="0"/>
    </radialGradient>
    <!-- 中部至正中央：微光霓虹洋红与梦幻紫罗兰，覆盖主视区中心 -->
    <radialGradient id="amb-rose" cx="48%" cy="58%" r="72%">
      <stop offset="0%" stop-color="#d946ef" stop-opacity="0.85"/>
      <stop offset="35%" stop-color="#ec4899" stop-opacity="0.6"/>
      <stop offset="70%" stop-color="#a855f7" stop-opacity="0.25"/>
      <stop offset="100%" stop-color="#a855f7" stop-opacity="0"/>
    </radialGradient>
    <!-- 右下角：清冽翡翠绿 -->
    <radialGradient id="amb-emerald" cx="85%" cy="85%" r="65%">
      <stop offset="0%" stop-color="#10b981" stop-opacity="0.85"/>
      <stop offset="45%" stop-color="#059669" stop-opacity="0.5"/>
      <stop offset="80%" stop-color="#10b981" stop-opacity="0.2"/>
      <stop offset="100%" stop-color="#10b981" stop-opacity="0"/>
    </radialGradient>
    <!-- 左下角：深邃冷海蓝，饱满托底 -->
    <radialGradient id="amb-ocean" cx="16%" cy="80%" r="65%">
      <stop offset="0%" stop-color="#2563eb" stop-opacity="0.85"/>
      <stop offset="50%" stop-color="#3b82f6" stop-opacity="0.45"/>
      <stop offset="100%" stop-color="#3b82f6" stop-opacity="0"/>
    </radialGradient>
  </defs>
  <rect width="100%" height="100%" fill="url(#amb-cyan)"/>
  <rect width="100%" height="100%" fill="url(#amb-indigo)"/>
  <rect width="100%" height="100%" fill="url(#amb-rose)"/>
  <rect width="100%" height="100%" fill="url(#amb-ocean)"/>
  <rect width="100%" height="100%" fill="url(#amb-emerald)"/>
</svg>
`.trim())}`

export interface PresetWallpaper {
  id: string
  labelKey: string
  dataUrl: string
}

export const PRESET_WALLPAPERS: PresetWallpaper[] = [
  {
    id: 'ambient',
    labelKey: 'settings.bg.presetAmbient',
    dataUrl: DEFAULT_WALLPAPER_DATA_URL,
  },
  {
    id: 'aurora',
    labelKey: 'settings.bg.presetAurora',
    dataUrl: `data:image/svg+xml;utf8,${encodeURIComponent(`
      <svg xmlns="http://www.w3.org/2000/svg" width="1920" height="1080" viewBox="0 0 1920 1080">
        <defs>
          <radialGradient id="g1" cx="30%" cy="25%" r="65%">
            <stop offset="0%" stop-color="#4f46e5" stop-opacity="0.85"/>
            <stop offset="100%" stop-color="#0f172a" stop-opacity="0"/>
          </radialGradient>
          <radialGradient id="g2" cx="75%" cy="65%" r="60%">
            <stop offset="0%" stop-color="#06b6d4" stop-opacity="0.75"/>
            <stop offset="100%" stop-color="#020617" stop-opacity="0"/>
          </radialGradient>
          <radialGradient id="g3" cx="60%" cy="15%" r="55%">
            <stop offset="0%" stop-color="#ec4899" stop-opacity="0.5"/>
            <stop offset="100%" stop-color="#0f172a" stop-opacity="0"/>
          </radialGradient>
        </defs>
        <rect width="100%" height="100%" fill="#080c14"/>
        <rect width="100%" height="100%" fill="url(#g1)"/>
        <rect width="100%" height="100%" fill="url(#g2)"/>
        <rect width="100%" height="100%" fill="url(#g3)"/>
      </svg>
    `.trim())}`,
  },
  {
    id: 'space',
    labelKey: 'settings.bg.presetSpace',
    dataUrl: `data:image/svg+xml;utf8,${encodeURIComponent(`
      <svg xmlns="http://www.w3.org/2000/svg" width="1920" height="1080" viewBox="0 0 1920 1080">
        <defs>
          <radialGradient id="s1" cx="50%" cy="40%" r="65%">
            <stop offset="0%" stop-color="#1e293b" stop-opacity="0.95"/>
            <stop offset="100%" stop-color="#020617" stop-opacity="0"/>
          </radialGradient>
          <radialGradient id="s2" cx="20%" cy="80%" r="55%">
            <stop offset="0%" stop-color="#2563eb" stop-opacity="0.45"/>
            <stop offset="100%" stop-color="#020617" stop-opacity="0"/>
          </radialGradient>
        </defs>
        <rect width="100%" height="100%" fill="#04060a"/>
        <rect width="100%" height="100%" fill="url(#s1)"/>
        <rect width="100%" height="100%" fill="url(#s2)"/>
      </svg>
    `.trim())}`,
  },
  {
    id: 'cyber',
    labelKey: 'settings.bg.presetCyber',
    dataUrl: `data:image/svg+xml;utf8,${encodeURIComponent(`
      <svg xmlns="http://www.w3.org/2000/svg" width="1920" height="1080" viewBox="0 0 1920 1080">
        <defs>
          <radialGradient id="c1" cx="25%" cy="30%" r="55%">
            <stop offset="0%" stop-color="#059669" stop-opacity="0.65"/>
            <stop offset="100%" stop-color="#020617" stop-opacity="0"/>
          </radialGradient>
          <radialGradient id="c2" cx="80%" cy="70%" r="65%">
            <stop offset="0%" stop-color="#0284c7" stop-opacity="0.75"/>
            <stop offset="100%" stop-color="#020617" stop-opacity="0"/>
          </radialGradient>
        </defs>
        <rect width="100%" height="100%" fill="#03090e"/>
        <rect width="100%" height="100%" fill="url(#c1)"/>
        <rect width="100%" height="100%" fill="url(#c2)"/>
      </svg>
    `.trim())}`,
  },
]

/**
 * 获取当前存储的壁纸设置
 * 若全新打开未配置，默认使用推荐配置；若用户曾主动关闭（opacity=0），则保持关闭尊重偏好
 */
export function getStoredWallpaperConfig() {
  try {
    const savedImg = localStorage.getItem('silencer_bg_image')
    const savedOpacity = localStorage.getItem('silencer_bg_opacity')
    const savedBlur = localStorage.getItem('silencer_bg_blur')
    const savedCardOpacity = localStorage.getItem('silencer_card_opacity')

    // 未做任何配置的全新状态（null），启用默认旗舰视觉体验
    const isInitial = savedImg === null && savedOpacity === null

    const bgImage = isInitial ? DEFAULT_WALLPAPER_DATA_URL : (savedImg || '')
    const bgOpacity = isInitial ? DEFAULT_OPACITY : (savedOpacity !== null ? Number(savedOpacity) : 0)
    const bgBlur = savedBlur !== null ? Number(savedBlur) : (isInitial ? DEFAULT_BLUR : 0)
    const parsedCardOpacity = savedCardOpacity === null ? DEFAULT_CARD_OPACITY : Number(savedCardOpacity)
    const cardOpacity = Number.isFinite(parsedCardOpacity)
      ? Math.max(0, Math.min(100, parsedCardOpacity))
      : DEFAULT_CARD_OPACITY

    return { bgImage, bgOpacity, bgBlur, cardOpacity }
  } catch {
    return {
      bgImage: DEFAULT_WALLPAPER_DATA_URL,
      bgOpacity: DEFAULT_OPACITY,
      bgBlur: DEFAULT_BLUR,
      cardOpacity: DEFAULT_CARD_OPACITY,
    }
  }
}
