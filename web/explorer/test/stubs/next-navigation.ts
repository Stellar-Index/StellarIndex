// Hermetic next/navigation stub for tests — returns inert router/query values
// so client components that call these hooks render without a Next runtime.
export const usePathname = () => '/';
export const useRouter = () => ({
  push: () => {},
  replace: () => {},
  prefetch: () => {},
  back: () => {},
  forward: () => {},
  refresh: () => {},
});
export const useSearchParams = () => new URLSearchParams();
export const useParams = (): Record<string, string> => ({});

export function redirect(): never {
  throw new Error('redirect() called during test');
}
export function notFound(): never {
  throw new Error('notFound() called during test');
}
