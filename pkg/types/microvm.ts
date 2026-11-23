// TypeScript data models for MicroVM Service
// These models mirror the Go models for frontend/client integration

export type MicroVMStatus = 'creating' | 'running' | 'stopping' | 'stopped' | 'error';

export type StorageType = 'local' | 's3-fuse' | 's3-nbd' | 'hybrid';

export type NetworkMode = 'dhcp' | 'macvlan' | 'nat' | 'ovn';

export type AvailabilityTier = 'high' | 'medium' | 'low';

export interface ResourceSpecs {
  vcpus: number;
  memory_mb: number;
  storage_mb: number;
}

export interface S3Config {
  bucket: string;
  region: string;
  access_key: string;
  secret_key: string;
}

export interface StorageConfig {
  type: StorageType;
  s3_config?: S3Config;
}

export interface NetworkConfig {
  mode: NetworkMode;
  custom_ip?: string;
}

export interface NetworkInfo {
  ip_address: string;
  mac_address: string;
  assignment_method: NetworkMode;
  ssh_port: number;
  public_key: string;
}

export interface PricingInfo {
  hourly_rate: number;
  currency: string;
}

export interface MicroVMMetadata {
  created_at: Date;
  last_activity: Date;
  customer_id: string;
}

export interface MicroVMInstance {
  id: string;
  node_id: string;
  status: MicroVMStatus;
  resources: ResourceSpecs;
  storage: StorageConfig;
  network: NetworkInfo;
  pricing: PricingInfo;
  metadata: MicroVMMetadata;
}

export interface ResourceCapacity {
  total_cpu: number;
  total_memory_mb: number;
  total_storage_mb: number;
}

export interface ResourceUsage {
  lxd_cpu: number;
  lxd_memory_mb: number;
  lxd_storage_mb: number;
  firecracker_cpu: number;
  firecracker_memory_mb: number;
  firecracker_storage_mb: number;
}

export interface ResourceAvailability {
  available_cpu: number;
  available_memory_mb: number;
  available_storage_mb: number;
}

export interface ResourcePool {
  node_id: string;
  capacity: ResourceCapacity;
  usage: ResourceUsage;
  availability: ResourceAvailability;
  last_updated: Date;
}

export interface BaseRates {
  cpu_per_hour: number;
  memory_mb_per_hour: number;
  storage_mb_per_hour: number;
}

export interface PricingMultipliers {
  cpu_multiplier: number;
  memory_multiplier: number;
  storage_multiplier: number;
}

export interface CurrentPricing {
  base_rates: BaseRates;
  current_multipliers: PricingMultipliers;
  availability_tier: AvailabilityTier;
  next_update_at: Date;
}

// Request/Response interfaces for API calls

export interface ProvisionMicroVMRequest {
  resources: ResourceSpecs;
  storage: StorageConfig;
  network: NetworkConfig;
  customer_id: string;
  preferred_node_id?: string;
}

export interface ProvisionMicroVMResponse {
  microvm?: MicroVMInstance;
  error_message?: string;
}

export interface GetMicroVMStatusRequest {
  microvm_id: string;
}

export interface GetMicroVMStatusResponse {
  microvm?: MicroVMInstance;
  error_message?: string;
}

export interface ListMicroVMsRequest {
  customer_id?: string;
  node_id?: string;
  status?: MicroVMStatus;
}

export interface ListMicroVMsResponse {
  microvms: MicroVMInstance[];
  error_message?: string;
}

export interface StopMicroVMRequest {
  microvm_id: string;
  force?: boolean;
}

export interface StopMicroVMResponse {
  success: boolean;
  error_message?: string;
}

export interface StartMicroVMRequest {
  microvm_id: string;
}

export interface StartMicroVMResponse {
  success: boolean;
  error_message?: string;
}

export interface DeleteMicroVMRequest {
  microvm_id: string;
  force?: boolean;
}

export interface DeleteMicroVMResponse {
  success: boolean;
  error_message?: string;
}

export interface GetCurrentPricingRequest {
  node_id?: string;
}

export interface GetCurrentPricingResponse {
  pricing?: CurrentPricing;
  error_message?: string;
}

export interface GetResourceAvailabilityRequest {
  node_id?: string;
}

export interface GetResourceAvailabilityResponse {
  resource_pools: ResourcePool[];
  error_message?: string;
}

// Utility functions for working with the models

export class MicroVMUtils {
  static isRunning(microvm: MicroVMInstance): boolean {
    return microvm.status === 'running';
  }

  static canBeStarted(microvm: MicroVMInstance): boolean {
    return microvm.status === 'stopped';
  }

  static canBeStopped(microvm: MicroVMInstance): boolean {
    return microvm.status === 'running';
  }

  static calculateHourlyPrice(specs: ResourceSpecs, pricing: CurrentPricing): number {
    const cpuCost = specs.vcpus * pricing.base_rates.cpu_per_hour * pricing.current_multipliers.cpu_multiplier;
    const memoryCost = specs.memory_mb * pricing.base_rates.memory_mb_per_hour * pricing.current_multipliers.memory_multiplier;
    const storageCost = specs.storage_mb * pricing.base_rates.storage_mb_per_hour * pricing.current_multipliers.storage_multiplier;

    return cpuCost + memoryCost + storageCost;
  }

  static getUtilizationPercentage(pool: ResourcePool): number {
    const totalUsedCPU = pool.usage.lxd_cpu + pool.usage.firecracker_cpu;
    const totalUsedMemory = pool.usage.lxd_memory_mb + pool.usage.firecracker_memory_mb;
    const totalUsedStorage = pool.usage.lxd_storage_mb + pool.usage.firecracker_storage_mb;

    const cpuUtil = totalUsedCPU / pool.capacity.total_cpu;
    const memUtil = totalUsedMemory / pool.capacity.total_memory_mb;
    const storageUtil = totalUsedStorage / pool.capacity.total_storage_mb;

    return (cpuUtil + memUtil + storageUtil) / 3.0;
  }

  static canAllocate(pool: ResourcePool, specs: ResourceSpecs): boolean {
    return pool.availability.available_cpu >= specs.vcpus &&
           pool.availability.available_memory_mb >= specs.memory_mb &&
           pool.availability.available_storage_mb >= specs.storage_mb;
  }

  static formatPrice(price: number, currency: string = 'USD'): string {
    return new Intl.NumberFormat('en-US', {
      style: 'currency',
      currency: currency,
      minimumFractionDigits: 4,
      maximumFractionDigits: 4
    }).format(price);
  }

  static formatResourceSize(sizeInMB: number): string {
    if (sizeInMB < 1024) {
      return `${sizeInMB} MB`;
    } else if (sizeInMB < 1024 * 1024) {
      return `${(sizeInMB / 1024).toFixed(1)} GB`;
    } else {
      return `${(sizeInMB / (1024 * 1024)).toFixed(1)} TB`;
    }
  }
}