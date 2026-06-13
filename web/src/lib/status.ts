import { Status } from '../gen/packetloss/v1/common_pb';

export function statusClass(s: Status): string {
  switch (s) {
    case Status.GREEN: return 's-green';
    case Status.AMBER: return 's-amber';
    case Status.RED: return 's-red';
    case Status.INSUFFICIENT_COVERAGE: return 's-na';
    default: return 's-none';
  }
}

export function statusLabel(s: Status): string {
  switch (s) {
    case Status.GREEN: return 'good';
    case Status.AMBER: return 'fair';
    case Status.RED: return 'poor';
    case Status.INSUFFICIENT_COVERAGE: return 'no coverage';
    default: return '—';
  }
}
