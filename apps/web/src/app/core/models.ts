export interface Workshop {
  id: string;
  code: string;
  name: string;
  track: string;
  instructor: string;
  workshopDate: string;
  priceCents: number;
  seatsTotal: number;
  seatsLeft: number;
  maxPerAttendee: number;
  kitsTotal: number;
  kitsLeft: number;
  kitPriceCents: number;
}

export type OrderStatus = 'Requested' | 'Held' | 'Sold' | 'Refunded' | 'Rejected';

export interface Order {
  id: string;
  workshopCode: string;
  ownerId: string;
  reference: string;
  seats: number;
  totalCents: number;
  kit: boolean;
  status: OrderStatus;
  reason: string | null;
  voucherNumber: string | null;
  createdAt: string;
  updatedAt: string;
  seatsReleasedAt: string | null;
}

export interface LogEntry {
  id: string;
  type: string;
  module: string;
  occurredAt: string;
}

export interface UserInfo {
  id: string;
  email: string;
  roles: string[];
}

export interface AuthResponse {
  accessToken: string;
  expiresAt: string;
  user: UserInfo;
}
