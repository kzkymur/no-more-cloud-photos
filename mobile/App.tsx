import { StatusBar } from 'expo-status-bar';
import { useCallback, useEffect, useRef, useState } from 'react';
import {
  ActivityIndicator,
  Alert,
  AppState,
  Platform,
  Pressable,
  ScrollView,
  StyleSheet,
  Switch,
  Text,
  TextInput,
  View,
} from 'react-native';
import { SafeAreaProvider, SafeAreaView } from 'react-native-safe-area-context';

import Uploader, {
  type FailedItem,
  type PermissionState,
  type UploaderConfig,
  type UploadStats,
} from './modules/nmcp-uploader/src/NmcpUploaderModule';

const POLL_MS = 1500;

export default function App() {
  return (
    <SafeAreaProvider>
      <SafeAreaView style={styles.root} edges={['top', 'bottom']}>
        <StatusBar style="dark" />
        <Main />
      </SafeAreaView>
    </SafeAreaProvider>
  );
}

function Main() {
  const [config, setConfig] = useState<UploaderConfig | null>(null);
  const [perm, setPerm] = useState<PermissionState | null>(null);
  const [editingServer, setEditingServer] = useState(false);

  const refreshPerm = useCallback(async () => setPerm(await Uploader.getPermissions()), []);

  useEffect(() => {
    (async () => {
      setConfig(await Uploader.getConfig());
      await refreshPerm();
    })();
  }, [refreshPerm]);

  // Coming back from system settings may have changed permissions;
  // coming back to the foreground is also a natural moment to pick up new photos.
  useEffect(() => {
    const sub = AppState.addEventListener('change', async (state) => {
      if (state !== 'active') return;
      const p = await Uploader.getPermissions();
      setPerm(p);
      const c = await Uploader.getConfig();
      if (c.serverUrl && c.autoUpload && (p.library === 'full' || p.library === 'partial')) {
        await Uploader.syncNow();
      }
    });
    return () => sub.remove();
  }, []);

  const saveConfig = useCallback(async (next: UploaderConfig) => {
    await Uploader.setConfig(next);
    setConfig(await Uploader.getConfig());
  }, []);

  if (!config || !perm) {
    return <ActivityIndicator style={{ marginTop: 64 }} />;
  }
  if (!config.serverUrl || editingServer) {
    return (
      <ServerSetup
        initial={config.serverUrl}
        onCancel={config.serverUrl ? () => setEditingServer(false) : undefined}
        onSaved={async (url) => {
          await saveConfig({ ...config, serverUrl: url });
          setEditingServer(false);
        }}
      />
    );
  }
  if (perm.library === 'undetermined' || perm.library === 'denied') {
    return <PermissionSetup perm={perm} onChange={setPerm} />;
  }
  return (
    <Dashboard
      config={config}
      perm={perm}
      onConfig={saveConfig}
      onEditServer={() => setEditingServer(true)}
      onPermRefresh={refreshPerm}
    />
  );
}

function ServerSetup(props: { initial: string; onSaved: (url: string) => void; onCancel?: () => void }) {
  const [url, setUrl] = useState(props.initial || 'https://');
  const [busy, setBusy] = useState(false);
  const [result, setResult] = useState<{ ok: boolean; text: string } | null>(null);

  const check = async () => {
    setBusy(true);
    setResult(null);
    try {
      const r = await Uploader.checkServer(url);
      setResult({
        ok: r.ok,
        text: r.ok ? '接続OK（/readyz）' : `接続できません${r.status ? `（HTTP ${r.status}）` : ''}: ${r.message}`,
      });
      return r.ok;
    } finally {
      setBusy(false);
    }
  };

  return (
    <ScrollView contentContainerStyle={styles.page} keyboardShouldPersistTaps="handled">
      <Text style={styles.title}>サーバ設定</Text>
      <Text style={styles.body}>
        自宅サーバ（Core API）のURLを入力してください。Tailscale の場合は例: https://photos.example.ts.net
      </Text>
      <TextInput
        style={styles.input}
        value={url}
        onChangeText={(t) => {
          setUrl(t);
          setResult(null);
        }}
        autoCapitalize="none"
        autoCorrect={false}
        keyboardType="url"
        placeholder="https://host.ts.net"
      />
      {result && <Text style={[styles.body, { color: result.ok ? '#137333' : '#b3261e' }]}>{result.text}</Text>}
      <View style={styles.row}>
        <Button title="接続確認" onPress={check} disabled={busy} secondary />
        <Button
          title="保存"
          disabled={busy}
          onPress={async () => {
            if (result?.ok || (await check())) props.onSaved(url.trim().replace(/\/+$/, ''));
          }}
        />
      </View>
      {busy && <ActivityIndicator />}
      {props.onCancel && <Button title="キャンセル" onPress={props.onCancel} secondary />}
    </ScrollView>
  );
}

function PermissionSetup(props: { perm: PermissionState; onChange: (p: PermissionState) => void }) {
  const denied = props.perm.library === 'denied';
  return (
    <ScrollView contentContainerStyle={styles.page}>
      <Text style={styles.title}>写真へのアクセス</Text>
      <Text style={styles.body}>
        ライブラリ内のすべての写真・動画を原本のままサーバへ送るため、「すべて許可」を選んでください。
        {Platform.OS === 'android' ? '撮影場所（位置情報）も残すため、位置情報へのアクセスも許可してください。' : ''}
      </Text>
      {denied ? (
        <>
          <Text style={[styles.body, styles.warnText]}>アクセスが拒否されています。設定アプリで許可してください。</Text>
          <Button title="設定を開く" onPress={() => Uploader.openSettings()} />
        </>
      ) : (
        <Button title="アクセスを許可" onPress={async () => props.onChange(await Uploader.requestPermissions())} />
      )}
    </ScrollView>
  );
}

function Dashboard(props: {
  config: UploaderConfig;
  perm: PermissionState;
  onConfig: (c: UploaderConfig) => Promise<void>;
  onEditServer: () => void;
  onPermRefresh: () => Promise<void>;
}) {
  const { config, perm } = props;
  const [stats, setStats] = useState<UploadStats | null>(null);
  const [failed, setFailed] = useState<FailedItem[]>([]);
  const [showFailed, setShowFailed] = useState(false);
  const started = useRef(false);

  useEffect(() => {
    let alive = true;
    const tick = async () => {
      try {
        const s = await Uploader.getStats();
        if (alive) setStats(s);
      } catch {
        // transient bridge errors are ignored; next tick retries
      }
    };
    tick();
    const timer = setInterval(tick, POLL_MS);
    return () => {
      alive = false;
      clearInterval(timer);
    };
  }, []);

  // First open of the dashboard: scan right away so the counts are meaningful.
  useEffect(() => {
    if (started.current) return;
    started.current = true;
    Uploader.syncNow();
  }, []);

  useEffect(() => {
    if (showFailed) Uploader.listFailed(200).then(setFailed);
  }, [showFailed, stats?.failed]);

  const progress = stats && stats.currentTotal > 0 ? stats.currentSent / stats.currentTotal : 0;

  return (
    <ScrollView contentContainerStyle={styles.page}>
      <Text style={styles.title}>NMCP Photos</Text>
      <Text style={styles.caption} numberOfLines={1}>
        {config.serverUrl}
      </Text>

      {perm.library === 'partial' && (
        <Warn
          text="一部の写真のみアクセスが許可されています。選択されていない写真はアップロードされません。"
          action="すべて許可する"
          onAction={async () => {
            await Uploader.extendPartialAccess();
            await props.onPermRefresh();
          }}
        />
      )}
      {!perm.mediaLocation && (
        <Warn
          text="アップロードを一時停止しています。写真の位置情報へのアクセスが許可されていないと、Android が撮影場所（GPS）を削除した状態でしか写真を読めないため、原本として送れません。"
          action="許可する"
          onAction={async () => {
            const next = await Uploader.requestPermissions();
            // Denied with "don't ask again": only the settings page can grant it.
            if (!next.mediaLocation) await Uploader.openSettings();
            await props.onPermRefresh();
          }}
        />
      )}

      <View style={styles.stats}>
        <Stat label="合計" value={stats?.total} />
        <Stat label="送信済" value={stats?.uploaded} color="#137333" />
        <Stat label="未送信" value={stats?.pending} color="#1a73e8" />
        <Stat label="失敗" value={stats?.failed} color={stats?.failed ? '#b3261e' : undefined} />
      </View>

      <View style={styles.card}>
        {stats?.running ? (
          <>
            <Text style={styles.body} numberOfLines={1}>
              送信中: {stats.currentName ?? 'ライブラリを確認中…'}
            </Text>
            <View style={styles.barTrack}>
              <View style={[styles.barFill, { width: `${Math.round(progress * 100)}%` }]} />
            </View>
            <Text style={styles.caption}>
              {formatBytes(stats.currentSent)} / {formatBytes(stats.currentTotal)}
            </Text>
          </>
        ) : (
          <Text style={styles.body}>
            {stats?.pending ? '待機中（条件が揃うと自動で送信します）' : 'すべて送信済みです'}
          </Text>
        )}
        {stats?.lastRunError ? <Text style={[styles.caption, styles.warnText]}>前回のエラー: {stats.lastRunError}</Text> : null}
        <Text style={styles.caption}>
          最終確認: {stats?.lastScanAt ? new Date(stats.lastScanAt).toLocaleString() : '—'}
        </Text>
      </View>

      <View style={styles.row}>
        <Button title="今すぐ同期" onPress={() => Uploader.syncNow()} />
        {stats?.running ? <Button title="停止" secondary onPress={() => Uploader.cancel()} /> : null}
      </View>

      {stats?.failed ? (
        <View style={styles.card}>
          <View style={styles.row}>
            <Button title="失敗を再試行" onPress={() => Uploader.retryFailed()} />
            <Button title={showFailed ? '一覧を閉じる' : '失敗一覧'} secondary onPress={() => setShowFailed(!showFailed)} />
          </View>
          {showFailed &&
            failed.map((f) => (
              <View key={f.id} style={styles.failedRow}>
                <Text style={styles.body} numberOfLines={1}>
                  {f.name}
                </Text>
                <Text style={styles.caption}>
                  {f.permanent ? '[サーバが拒否] ' : ''}
                  {f.error}（{f.attempts}回）
                </Text>
              </View>
            ))}
        </View>
      ) : null}

      <View style={styles.card}>
        <Toggle
          label="自動アップロード"
          value={config.autoUpload}
          onChange={(v) => props.onConfig({ ...config, autoUpload: v })}
        />
        <Toggle
          label="Wi-Fi接続時のみ"
          value={config.wifiOnly}
          onChange={(v) => props.onConfig({ ...config, wifiOnly: v })}
        />
        <Toggle
          label="充電中のみ"
          value={config.chargingOnly}
          onChange={(v) => props.onConfig({ ...config, chargingOnly: v })}
        />
        <Text style={styles.caption}>「今すぐ同期」は Wi-Fi/充電の条件に関係なく実行します。</Text>
      </View>

      <Button title="サーバURLを変更" secondary onPress={props.onEditServer} />
      {!perm.notifications && Platform.OS === 'android' ? (
        <Text style={styles.caption}>通知を許可すると、送信中の進捗が通知に表示されます。</Text>
      ) : null}
    </ScrollView>
  );
}

function Stat(props: { label: string; value?: number; color?: string }) {
  return (
    <View style={styles.stat}>
      <Text style={[styles.statValue, props.color ? { color: props.color } : null]}>{props.value ?? '—'}</Text>
      <Text style={styles.caption}>{props.label}</Text>
    </View>
  );
}

function Toggle(props: { label: string; value: boolean; onChange: (v: boolean) => void }) {
  return (
    <View style={styles.toggle}>
      <Text style={styles.body}>{props.label}</Text>
      <Switch value={props.value} onValueChange={props.onChange} />
    </View>
  );
}

function Warn(props: { text: string; action: string; onAction: () => void }) {
  return (
    <View style={styles.warn}>
      <Text style={styles.body}>{props.text}</Text>
      <Button title={props.action} secondary onPress={props.onAction} />
    </View>
  );
}

function Button(props: { title: string; onPress: () => unknown; secondary?: boolean; disabled?: boolean }) {
  return (
    <Pressable
      disabled={props.disabled}
      onPress={async () => {
        try {
          await props.onPress();
        } catch (e) {
          Alert.alert('エラー', e instanceof Error ? e.message : String(e));
        }
      }}
      style={({ pressed }) => [
        styles.button,
        props.secondary ? styles.buttonSecondary : null,
        (pressed || props.disabled) && { opacity: 0.6 },
      ]}
    >
      <Text style={[styles.buttonText, props.secondary ? styles.buttonTextSecondary : null]}>{props.title}</Text>
    </Pressable>
  );
}

function formatBytes(n: number): string {
  if (!n) return '0 B';
  const units = ['B', 'KB', 'MB', 'GB'];
  let i = 0;
  let v = n;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v.toFixed(i === 0 ? 0 : 1)} ${units[i]}`;
}

const styles = StyleSheet.create({
  root: { flex: 1, backgroundColor: '#f6f7f9' },
  page: { padding: 20, gap: 14 },
  title: { fontSize: 24, fontWeight: '700', color: '#111' },
  body: { fontSize: 15, color: '#222', lineHeight: 21 },
  caption: { fontSize: 12, color: '#666' },
  warnText: { color: '#b3261e' },
  input: {
    borderWidth: 1,
    borderColor: '#c4c7c5',
    borderRadius: 8,
    paddingHorizontal: 12,
    paddingVertical: 10,
    fontSize: 16,
    backgroundColor: '#fff',
  },
  row: { flexDirection: 'row', gap: 10, flexWrap: 'wrap', alignItems: 'center' },
  stats: { flexDirection: 'row', gap: 8 },
  stat: { flex: 1, backgroundColor: '#fff', borderRadius: 10, paddingVertical: 12, alignItems: 'center' },
  statValue: { fontSize: 22, fontWeight: '700', color: '#111' },
  card: { backgroundColor: '#fff', borderRadius: 10, padding: 14, gap: 8 },
  warn: { backgroundColor: '#fff4e5', borderRadius: 10, padding: 14, gap: 8, borderColor: '#f9ab00', borderWidth: 1 },
  barTrack: { height: 8, backgroundColor: '#e3e3e3', borderRadius: 4, overflow: 'hidden' },
  barFill: { height: 8, backgroundColor: '#1a73e8' },
  toggle: { flexDirection: 'row', justifyContent: 'space-between', alignItems: 'center' },
  failedRow: { borderTopWidth: StyleSheet.hairlineWidth, borderTopColor: '#ddd', paddingTop: 6 },
  button: { backgroundColor: '#1a73e8', paddingHorizontal: 16, paddingVertical: 10, borderRadius: 8 },
  buttonSecondary: { backgroundColor: '#e8f0fe' },
  buttonText: { color: '#fff', fontWeight: '600', fontSize: 15 },
  buttonTextSecondary: { color: '#1a73e8' },
});
