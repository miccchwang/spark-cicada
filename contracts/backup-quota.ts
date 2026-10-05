/**
 * contracts/backup-quota.ts —— v1.0
 *
 * 备份与容灾契约（第三轮拍板新增）。
 *
 * 落地：
 *   - B1 双云主备（主：阿里云·新加坡；备：AWS·美国）+ 跨云异步同步
 *   - B2 备份云保留 180 天数据回滚能力
 *   - B3 云数据可本地备份 + 下载；每月 1 号自动备份上一个月；每月限 1 次下载
 *
 * 详见 docs/04-运维方案.md §6
 */

export const BACKUP_QUOTA_VERSION = "1.0" as const;

/* ───────────────────── 双云主备（B1） ───────────────────── */

export type SiteRole = "PRIMARY" | "STANDBY";

export interface CloudSite {
  role: SiteRole;
  /** 云厂商：aliyun / aws */
  provider: "aliyun" | "aws";
  /** 区域，如 ap-southeast-1 / us-east-1 */
  region: string;
  /** 该站点的组件 */
  components: ("app" | "compute" | "db" | "cache" | "web")[];
}

export interface ReplicationConfig {
  /** 同步方式：异步（本项目默认） */
  mode: "ASYNC" | "SYNC";
  /** 机制：WAL 流式 / 逻辑复制 / 定期快照 */
  mechanism: ("wal_stream" | "logical" | "snapshot")[];
  /** 传输通道：加密隧道 */
  channel: "wireguard" | "cloud_direct";
  /** 同步延迟告警阈值（秒） */
  lagAlertSeconds: number;
  /** 预期 RPO 描述（异步 ⇒ 秒级~分钟） */
  rpo: string;
}

/* ───────────────────── 180 天回滚（B2） ───────────────────── */

export interface RollbackPolicy {
  /** 回滚窗口（天） */
  windowDays: number;
  /** 回滚粒度：按天 */
  granularity: "day";
  /** 谁能发起：仅 T1 */
  initiatorTier: "T1";
  /** 是否二次确认 */
  requireSecondConfirm: boolean;
  /** 回滚前是否先快照当前状态 */
  snapshotBeforeRollback: boolean;
  /** 永不回滚的对象（审计表） */
  excludedTables: string[];
}

export const DEFAULT_ROLLBACK_POLICY: RollbackPolicy = {
  windowDays: 180,
  granularity: "day",
  initiatorTier: "T1",
  requireSecondConfirm: true,
  snapshotBeforeRollback: true,
  excludedTables: ["audit_log"],
};

/* ───────────────── 本地备份下载配额（B3） ───────────────── */

export interface BackupArchive {
  id: string;
  /** 数据月份，如 2026-09 */
  dataMonth: string;
  /** 生成时间（每月 1 号自动生成上月归档） */
  generatedAt: string;
  sizeBytes: number;
  /** 归档校验和 */
  sha256: string;
  /** 一次性签名 URL（短时效，**不含凭据**，符合 docs/09） */
  signedUrl?: string;
  /** 签名过期时间 */
  urlExpiresAt?: string;
}

export interface BackupDownloadQuota {
  v: typeof BACKUP_QUOTA_VERSION;
  account: string;
  /** 自然月（YYYY-MM） */
  period: string;
  /** 本月已用下载次数 */
  used: number;
  /** 本月上限（默认 1） */
  limit: number;
  /** 下一次可下载时间 */
  nextAvailableAt: string;
  /** 可下载的归档列表 */
  archives: BackupArchive[];
}

/** 下载校验结果 */
export interface DownloadCheckResult {
  allowed: boolean;
  reason?: "QUOTA_EXCEEDED" | "NOT_AUTHORIZED" | "ARCHIVE_NOT_FOUND" | "NOT_YET_AVAILABLE";
  /** 拒绝时给出下次可下载时间 */
  nextAvailableAt?: string;
}
