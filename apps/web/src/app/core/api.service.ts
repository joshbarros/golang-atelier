import { HttpClient } from '@angular/common/http';
import { Injectable, inject } from '@angular/core';
import { firstValueFrom } from 'rxjs';
import { Order, LogEntry, Workshop } from './models';

export interface NewWorkshop {
  code: string;
  name: string;
  track: string;
  instructor: string;
  seatsTotal: number;
  maxPerAttendee: number;
  kitsTotal: number;
  kitPriceCents: number;
  priceCents: number;
}

@Injectable({ providedIn: 'root' })
export class ApiService {
  private readonly http = inject(HttpClient);

  workshops = () => firstValueFrom(this.http.get<Workshop[]>('/api/workshops'));
  openWorkshop = (body: NewWorkshop) =>
    firstValueFrom(this.http.post<Workshop>('/api/workshops', body));

  orders = (workshopCode: string) =>
    firstValueFrom(this.http.get<Order[]>('/api/orders', { params: { workshopCode } }));
  book = (body: { workshopCode: string; reference: string; seats: number; kit: boolean }) =>
    firstValueFrom(this.http.post<Order>('/api/orders', body));
  refund = (id: string) => firstValueFrom(this.http.post<Order>(`/api/orders/${id}/refund`, {}));
  events = (id: string) => firstValueFrom(this.http.get<LogEntry[]>(`/api/orders/${id}/events`));
}
