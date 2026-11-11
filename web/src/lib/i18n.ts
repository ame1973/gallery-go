import { authMessages } from '../features/auth/messages';
import { libraryMessages } from '../features/library/messages';

export const locales = ['en', 'zh-TW', 'zh-CN'] as const;
export type Locale = (typeof locales)[number];

const en = {
  ...authMessages.en,
  ...libraryMessages.en,
  language: 'Language',
  title: 'Service connection',
  description: 'A private place for the photographs and films you keep.',
  checking: 'Checking the service…',
  connected: 'The service is ready.',
  degraded: 'The service is available, but media sources need attention.',
  failed: 'The service is not ready. Check the server and required workers, then try again.',
  worker: 'Background processing',
  sources: 'Media sources',
  optionalWorker: 'Not required',
  needsAttention: 'Needs attention',
  retry: 'Check again',
  api: 'HTTP service',
  database: 'Metadata database',
  healthy: 'Connected',
  version: 'Version {version}',
  development: 'Development build',
  developmentNote:
    'Account security and album browsing are available. Media processing, the full viewer, and offline downloads are still under development.',
  privacy: 'Your originals stay yours.',
  privacyNote: 'Source media is read-only. This page contains no private gallery data.',
  footer: 'GalleryGo · Self-hosted, by design',
};

type Messages = Record<keyof typeof en, string>;

export const messages: Record<Locale, Messages> = {
  en,
  'zh-TW': {
    ...authMessages['zh-TW'],
    ...libraryMessages['zh-TW'],
    language: '語言',
    title: '服務連線',
    description: '為你珍藏的照片與影片，留一個私密的地方。',
    checking: '正在檢查服務…',
    connected: '服務已就緒。',
    degraded: '服務可用，但媒體來源需檢查。',
    failed: '服務尚未就緒。請檢查伺服器及必要的背景工作程序後再試一次。',
    worker: '背景處理',
    sources: '媒體來源',
    optionalWorker: '非必要',
    needsAttention: '需檢查',
    retry: '重新檢查',
    api: 'HTTP 服務',
    database: '中繼資料庫',
    healthy: '已連線',
    version: '版本 {version}',
    development: '開發版本',
    developmentNote: '已提供帳號安全與相簿瀏覽。媒體處理、完整檢視器及離線下載仍在開發中。',
    privacy: '原始檔案，始終屬於你。',
    privacyNote: '來源媒體維持唯讀。此頁面不包含任何私人圖庫資料。',
    footer: 'GalleryGo · 為自行託管而設計',
  },
  'zh-CN': {
    ...authMessages['zh-CN'],
    ...libraryMessages['zh-CN'],
    language: '语言',
    title: '服务连接',
    description: '为你珍藏的照片与影片，留一个私密的地方。',
    checking: '正在检查服务…',
    connected: '服务已就绪。',
    degraded: '服务可用，但媒体来源需要检查。',
    failed: '服务尚未就绪。请检查服务器及必要的后台工作进程后重试。',
    worker: '后台处理',
    sources: '媒体来源',
    optionalWorker: '非必需',
    needsAttention: '需检查',
    retry: '重新检查',
    api: 'HTTP 服务',
    database: '元数据库',
    healthy: '已连接',
    version: '版本 {version}',
    development: '开发版本',
    developmentNote: '已提供账号安全与相册浏览。媒体处理、完整查看器及离线下载仍在开发中。',
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
