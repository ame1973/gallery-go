export const locales = ['en', 'zh-TW', 'zh-CN'] as const;
export type Locale = (typeof locales)[number];

const en = {
  language: 'Language',
  title: 'Service connection',
  description: 'A private place for the photographs and films you keep.',
  checking: 'Checking the service…',
  connected: 'The service is ready.',
  failed: 'The service could not be reached. Check the server, then try again.',
  retry: 'Check again',
  api: 'HTTP service',
  database: 'Metadata database',
  healthy: 'Connected',
  version: 'Version {version}',
  development: 'Development build',
  developmentNote:
    'The foundation is running. Account setup, media browsing, and offline downloads are not available in this build yet.',
  privacy: 'Your originals stay yours.',
  privacyNote: 'Source media is read-only. This page contains no private gallery data.',
  footer: 'GalleryGo · Self-hosted, by design',
};

type Messages = Record<keyof typeof en, string>;

export const messages: Record<Locale, Messages> = {
  en,
  'zh-TW': {
    language: '語言',
    title: '服務連線',
    description: '為你珍藏的照片與影片，留一個私密的地方。',
    checking: '正在檢查服務…',
    connected: '服務已就緒。',
    failed: '無法連線至服務。請檢查伺服器後再試一次。',
    retry: '重新檢查',
    api: 'HTTP 服務',
    database: '中繼資料庫',
    healthy: '已連線',
    version: '版本 {version}',
    development: '開發版本',
    developmentNote: '基礎服務已啟動。此版本尚未提供帳號設定、媒體瀏覽及離線下載。',
    privacy: '原始檔案，始終屬於你。',
    privacyNote: '來源媒體維持唯讀。此頁面不包含任何私人圖庫資料。',
    footer: 'GalleryGo · 為自行託管而設計',
  },
  'zh-CN': {
    language: '语言',
    title: '服务连接',
    description: '为你珍藏的照片与影片，留一个私密的地方。',
    checking: '正在检查服务…',
    connected: '服务已就绪。',
    failed: '无法连接到服务。请检查服务器后再试一次。',
    retry: '重新检查',
    api: 'HTTP 服务',
    database: '元数据库',
    healthy: '已连接',
    version: '版本 {version}',
    development: '开发版本',
    developmentNote: '基础服务已启动。此版本尚未提供账号设置、媒体浏览及离线下载。',
    privacy: '原始文件，始终属于你。',
    privacyNote: '来源媒体保持只读。此页面不包含任何私人图库数据。',
    footer: 'GalleryGo · 为自行托管而设计',
  },
};

export function initialLocale(): Locale {
  try {
    const stored = localStorage.getItem('gallerygo:locale');
    return locales.find((locale) => locale === stored) ?? 'en';
  } catch {
    return 'en';
  }
}
