import {
  LendingProtocolPage,
  lendingProtocolMetadata,
} from '../LendingProtocolPage';

export const metadata = lendingProtocolMetadata('blend');

export default function BlendLendingPage() {
  return <LendingProtocolPage protocol="blend" />;
}
