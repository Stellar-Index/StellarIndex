import {
  LendingProtocolPage,
  lendingProtocolMetadata,
} from '../LendingProtocolPage';

export const metadata = lendingProtocolMetadata('blnt');

export default function BlntLendingPage() {
  return <LendingProtocolPage protocol="blnt" />;
}
