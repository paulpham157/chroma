mod assignment;
mod compactor;
mod memberlist;
mod server;
mod tracing;
mod utils;

use chroma_config::Configurable;
use memberlist::MemberlistProvider;

use tokio::select;
use tokio::signal::unix::{signal, SignalKind};

// Required for benchmark
pub mod config;
pub mod execution;
pub mod log;
pub mod segment;

const CONFIG_PATH_ENV_VAR: &str = "CONFIG_PATH";

pub async fn query_service_entrypoint() {
    // Check if the config path is set in the env var
    let config = match std::env::var(CONFIG_PATH_ENV_VAR) {
        Ok(config_path) => config::RootConfig::load_from_path(&config_path),
        Err(_) => config::RootConfig::load(),
    };

    let config = config.query_service;

    crate::tracing::opentelemetry_config::init_otel_tracing(
        &config.service_name,
        &config.otel_endpoint,
    );

    let system = chroma_system::System::new();
    let dispatcher = match chroma_system::Dispatcher::try_from_config(&config.dispatcher).await {
        Ok(dispatcher) => dispatcher,
        Err(err) => {
            println!("Failed to create dispatcher component: {:?}", err);
            return;
        }
    };
    let mut dispatcher_handle = system.start_component(dispatcher);
    let mut worker_server = match server::WorkerServer::try_from_config(&config).await {
        Ok(worker_server) => worker_server,
        Err(err) => {
            println!("Failed to create worker server component: {:?}", err);
            return;
        }
    };
    worker_server.set_system(system.clone());
    worker_server.set_dispatcher(dispatcher_handle.clone());

    let server_join_handle = tokio::spawn(async move {
        let _ = crate::server::WorkerServer::run(worker_server).await;
    });

    let mut sigterm = match signal(SignalKind::terminate()) {
        Ok(sigterm) => sigterm,
        Err(e) => {
            println!("Failed to create signal handler: {:?}", e);
            return;
        }
    };

    println!("Waiting for SIGTERM to stop the server");
    select! {
        // Kubernetes will send SIGTERM to stop the pod gracefully
        // TODO: add more signal handling
        _ = sigterm.recv() => {
            dispatcher_handle.stop();
            let _ = dispatcher_handle.join().await;
            system.stop().await;
            system.join().await;
            let _ = server_join_handle.await;
        },
    };
    println!("Server stopped");
}

pub async fn compaction_service_entrypoint() {
    // Check if the config path is set in the env var
    let config = match std::env::var(CONFIG_PATH_ENV_VAR) {
        Ok(config_path) => config::RootConfig::load_from_path(&config_path),
        Err(_) => config::RootConfig::load(),
    };

    let config = config.compaction_service;

    crate::tracing::opentelemetry_config::init_otel_tracing(
        &config.service_name,
        &config.otel_endpoint,
    );

    let system = chroma_system::System::new();

    let mut memberlist = match memberlist::CustomResourceMemberlistProvider::try_from_config(
        &config.memberlist_provider,
    )
    .await
    {
        Ok(memberlist) => memberlist,
        Err(err) => {
            println!("Failed to create memberlist component: {:?}", err);
            return;
        }
    };

    let dispatcher = match chroma_system::Dispatcher::try_from_config(&config.dispatcher).await {
        Ok(dispatcher) => dispatcher,
        Err(err) => {
            println!("Failed to create dispatcher component: {:?}", err);
            return;
        }
    };
    let mut dispatcher_handle = system.start_component(dispatcher);
    let mut compaction_manager =
        match crate::compactor::CompactionManager::try_from_config(&config).await {
            Ok(compaction_manager) => compaction_manager,
            Err(err) => {
                println!("Failed to create compaction manager component: {:?}", err);
                return;
            }
        };
    compaction_manager.set_dispatcher(dispatcher_handle.clone());
    compaction_manager.set_system(system.clone());

    let mut compaction_manager_handle = system.start_component(compaction_manager);
    memberlist.subscribe(compaction_manager_handle.receiver());

    let mut memberlist_handle = system.start_component(memberlist);

    let mut sigterm = match signal(SignalKind::terminate()) {
        Ok(sigterm) => sigterm,
        Err(e) => {
            println!("Failed to create signal handler: {:?}", e);
            return;
        }
    };
    println!("Waiting for SIGTERM to stop the server");
    select! {
        // Kubernetes will send SIGTERM to stop the pod gracefully
        // TODO: add more signal handling
        _ = sigterm.recv() => {
            memberlist_handle.stop();
            let _ = memberlist_handle.join().await;
            dispatcher_handle.stop();
            let _ = dispatcher_handle.join().await;
            compaction_manager_handle.stop();
            let _ = compaction_manager_handle.join().await;
            system.stop().await;
            system.join().await;
        },
    };
    println!("Server stopped");
}
