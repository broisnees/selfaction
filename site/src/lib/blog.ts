import { getCollection, type CollectionEntry } from 'astro:content';

export type Post = CollectionEntry<'blog'>;

/** Posts, newest first. */
export async function posts(): Promise<Post[]> {
	return (await getCollection('blog')).sort((a, b) => b.data.date.getTime() - a.data.date.getTime());
}

export const formatDate = (d: Date) =>
	d.toLocaleDateString('en-US', { year: 'numeric', month: 'long', day: 'numeric', timeZone: 'UTC' });

/** Reading time at about 230 words a minute, never less than one. */
export const minutes = (body = '') => Math.max(1, Math.round(body.split(/\s+/).filter(Boolean).length / 230));
