/**
 * contracts/backup-quota.ts —— v1.0
 *
 * 备份与容灾契约（第三轮拍板新增）。
 *
 * 落地：
 *   - B1 **双地域 × 每地域主备**：新加坡（东南亚）/ 美国（美区）各一套「阿里云主 + AWS 备」
 *        + **地域内异步同步**（非跨洋主备）
 *   - B2 **分地域**保留 180 天数据回滚能力
 *   - B3 云数据可本地备份 + 下载；每月 1 号自动备份上一个月；每月限 1 次下载（分地域计数）
 *
 * 详见 docs/04-运维方案.md §6
 */

export const BACKUP_QUOTA_VERSION = "1.0" as const;

/* ───────────────────── 双地域 × 每地域主备（B1） ───────────────────── */

export type SiteRole = "PRIMARY" | "STANDBY";

/** 地域：新加坡（东南亚业务）/ 美国（美区业务） */
export type Region = "ap-southeast-1" | "us-east-1";

export interface CloudSite {
  /** 地域 */
  region: Region;
  /** 该地域服务的业务范围 */
  businessScope: string;        // 如 "东南亚" / "美区"
  role: SiteRole;
  /** 云厂商：aliyun / aws（每地域：阿里云主 + AWS 备） */
  provider: "aliyun" | "aws";
  /** 该站点的组件 */
  components: ("app" | "compute" | "db" | "cache" | "web")[];
}

/**
 * 部署拓扑：两个地域，每个地域内部各一套主备。
 * 地域之间业务分区、互不互为实时备（仅汇总视图 / 灾备归档）。
 */
export interface DeploymentTopology {
  v: typeof BACKUP_QUOTA_VERSION;
  regions: {
    region: Region;
    business: "东南亚" | "美区";
    primary: CloudSite;         // 阿里云（该地域）
    standby: CloudSite;         // AWS（同地域）
    /** 该地域承接的渠道/店铺（按业务分区） */
    channels: string[];
  }[];
  /** 地域间关系：业务分区（非主备） */
  interRegionMode: "BUSINESS_PARTITION";
  /** 是否允许跨地域写同一行 */
  allowCrossRegionWrite: false;
}

/** 同步配置：地域内（主→备）异步 */
export interface ReplicationConfig {
  /** 同步范围：同地域内 */
  scope: "INTRA_REGION";
  /** 同步方式：异步（本项目默认） */
  mode: "ASYNC" | "SYNC";
  /** 机制：WAL 流式 / 逻辑复制 / 定期快照 */
  mechanism: ("wal_stream" | "logical" | "snapshot")[];
  /** 传输通道：同地域内网加密 */
  channel: "intra_region_encrypted" | "wireguard" | "cloud_direct";
  /** 同步延迟告警阈值（秒）——同地域可更低 */
  lagAlertSeconds: number;
  /** 预期 RPO 描述（同地域异步 ⇒ 秒级） */
  rpo: string;
}

/* ───────────────────── 180 天回滚（B2） ───────────────────── */

export interface RollbackPolicy {
  /** 回滚窗口（天） */
  windowDays: number;
  /** 范围：各地域独立（不跨地域回滚） */
  scope: "PER_REGION";
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
  scope: "PER_REGION",
  granularity: "day",
  initiatorTier: "T1",
  requireSecondConfirm: true,
  snapshotBeforeRollback: true,
  excludedTables: ["audit_log"],
};

/* ───────────────── 本地备份下载配额（B3） ───────────────── */

export interface BackupArchive {
  id: string;
  /** 所属地域 */
  region: Region;
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
  /** 地域（配额按地域分别计数） */
  region?: Region;
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
