// Built-in plugin catalog displayed by the install wizard. SmoothNAS
// intentionally stores only repository pointers here; manifests are
// resolved from each repo's latest GitHub release at install time.

export type CatalogRepository = {
  id: string;
  repo: string;
};

export const pluginCatalogRepositories: CatalogRepository[] = [
  { id: 'gh-runner', repo: 'RakuenSoftware/smoothnas-plugin-gh-runner' },
  { id: 'llama-cpp', repo: 'RakuenSoftware/smoothnas-plugin-llama-cpp' },
  { id: 'wolf', repo: 'RakuenSoftware/smoothnas-plugin-wolf' },
];
