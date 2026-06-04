// Built-in plugin catalog displayed by the install wizard. SmoothNAS
// intentionally stores only repository pointers here; manifests are
// resolved from each repo's latest GitHub release at install time.

export type CatalogRepository = {
  id: string;
  repo: string;
};

export const pluginCatalogRepositories: CatalogRepository[] = [
  // One repo, three manifests (aimee-server / aimee-kb / aimee-combined);
  // buildCatalogEntries renders one card per manifest name.
  { id: 'aimee', repo: 'RakuenSoftware/smoothnas-plugin-aimee' },
  { id: 'gh-runner', repo: 'RakuenSoftware/smoothnas-plugin-gh-runner' },
  { id: 'llama-cpp', repo: 'RakuenSoftware/smoothnas-plugin-llama-cpp' },
  { id: 'vllm', repo: 'RakuenSoftware/smoothnas-plugin-vllm' },
  { id: 'wolf', repo: 'RakuenSoftware/smoothnas-plugin-wolf' },
];
